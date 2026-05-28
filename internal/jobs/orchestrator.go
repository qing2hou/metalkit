// Orchestrator drives the side of the system that needs goroutines and IPC:
// scan bindings whose desired_state is install/reinstall, materialise a job,
// poke the BMC to (re)PXE, and on job completion flip the binding back to
// desired_state=none and tell the BMC to boot from disk next.
//
// What the orchestrator does NOT do (intentionally):
//   - run actual installs — that's the agent on the live-boot machine
//   - parse log streams — log lines are stored as-is by AppendLog
//   - retry failed jobs — admins explicitly Create a retry from the UI
//
// Lifecycle: a single goroutine started from main. tickInterval governs scan
// frequency; default 5s is well below the BMC reboot + PXE round trip so the
// orchestrator can react quickly to UI-driven state changes without thrashing
// the DB.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// BMCFetcher decouples the orchestrator from the bmc package; both packages
// would otherwise need to coordinate on a Go-level interface. We pull just
// "given a machine_uuid, return the BMC credential bundle (with plaintext
// password) or an error" — exactly what the ipmi wrapper needs.
type BMCFetcher interface {
	GetWithPassword(ctx context.Context, machineUUID string) (BMCCredential, error)
}

// BMCCredential is the orchestrator-facing slice of bmc.PasswordedCredential.
// Defined here so the jobs package can talk to ipmi without importing bmc
// (avoids a cycle once bmc grows to consume jobs for audit).
type BMCCredential struct {
	IP            string
	Port          int
	Username      string
	Password      string
	IPMIInterface string
}

// IPMIClient is the orchestrator-facing slice of *ipmi.Client. Same reason:
// avoid importing ipmi from jobs; main.go bridges concrete types into here.
type IPMIClient interface {
	BootForPXE(ctx context.Context, cred BMCCredential) error
	FinalizeBootDisk(ctx context.Context, cred BMCCredential) error
}

// BindingUpdater lets the orchestrator clear desired_state after a successful
// install. Mirrors bindings.Store.UpdateDesiredState (the actual signature
// will be added to bindings if it doesn't exist yet).
type BindingUpdater interface {
	ClearDesiredState(ctx context.Context, machineUUID, updatedBy string) error
}

// OrchestratorConfig configures NewOrchestrator.
type OrchestratorConfig struct {
	Store          *Store
	BMC            BMCFetcher
	IPMI           IPMIClient
	Bindings       BindingUpdater
	Logger         *slog.Logger
	TickInterval   time.Duration // default 5s
	BMCActor       string        // who to record as updated_by on cleared bindings; default "orchestrator"
}

// Orchestrator runs the binding→job→BMC reconciliation loop.
type Orchestrator struct {
	cfg OrchestratorConfig
	db  *sql.DB // borrowed from Store for the scan query
}

// NewOrchestrator builds an Orchestrator. The Store is required; BMC / IPMI /
// Bindings are required for the reconciliation loop to actually do anything,
// but the constructor tolerates nils so unit tests can exercise the scan path
// without wiring full mocks (the loop logs and skips when prerequisites are
// nil).
func NewOrchestrator(cfg OrchestratorConfig) (*Orchestrator, error) {
	if cfg.Store == nil {
		return nil, errors.New("jobs: orchestrator needs Store")
	}
	if cfg.Logger == nil {
		return nil, errors.New("jobs: orchestrator needs Logger")
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = 5 * time.Second
	}
	if cfg.BMCActor == "" {
		cfg.BMCActor = "orchestrator"
	}
	return &Orchestrator{cfg: cfg, db: cfg.Store.db}, nil
}

// Run blocks until ctx is cancelled. Tick-driven; one scan per tick. Any
// per-machine error is logged and swallowed so one bad row doesn't wedge the
// loop for the rest.
func (o *Orchestrator) Run(ctx context.Context) {
	o.cfg.Logger.Info("orchestrator: starting", "tick", o.cfg.TickInterval)
	t := time.NewTicker(o.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			o.cfg.Logger.Info("orchestrator: stopped")
			return
		case <-t.C:
			o.tick(ctx)
		}
	}
}

// tick performs one reconciliation pass:
//
//  1. Find bindings with desired_state ∈ {install, reinstall} and no in-flight
//     job → Create a pending job.
//  2. For each pending job (just created or already there): fetch BMC creds,
//     SetBootDevice=pxe + PowerCycle, transition pending→running.
//  3. For each succeeded job whose binding still has desired_state != none:
//     FinalizeBootDisk + clear desired_state. (We DON'T touch the BMC if the
//     binding was already cleared — that means the operator or a previous
//     tick already finalized.)
//
// All errors are logged; nothing fatal at the loop level.
func (o *Orchestrator) tick(ctx context.Context) {
	o.handleInstallRequests(ctx)
	o.handleSucceededJobs(ctx)
}

func (o *Orchestrator) handleInstallRequests(ctx context.Context) {
	// A new job for a binding is created only when no job exists for the
	// binding since the binding's last update. This means:
	//   - in-flight (pending/running) jobs block — wait for them
	//   - terminal jobs (succeeded/failed/cancelled) created on or after the
	//     binding's updated_at also block — they represent the outcome of
	//     this binding revision; admin must touch the binding (re-PUT) to
	//     ask for another attempt. This implements the "no auto-retry of
	//     failed jobs" rule stated at the top of this file: a single
	//     mid-install glitch must NOT put the BMC into a PXE reboot loop.
	rows, err := o.db.QueryContext(ctx, `
        SELECT b.machine_uuid, b.image_id, b.profile_id, b.desired_state, b.updated_by
        FROM bindings b
        WHERE b.desired_state IN ('install', 'reinstall')
          AND NOT EXISTS (
              SELECT 1 FROM jobs j
              WHERE j.machine_uuid = b.machine_uuid
                AND (
                    j.status IN ('pending', 'running')
                    OR (j.status IN ('succeeded', 'failed', 'cancelled')
                        AND (j.created_at >= b.updated_at OR j.finished_at >= b.updated_at))
                )
          )`)
	if err != nil {
		o.cfg.Logger.Error("orchestrator: scan bindings", "err", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var muuid, imageID, profileID, desired, updatedBy string
		if err := rows.Scan(&muuid, &imageID, &profileID, &desired, &updatedBy); err != nil {
			o.cfg.Logger.Error("orchestrator: scan row", "err", err)
			continue
		}
		o.startJobForBinding(ctx, muuid, imageID, profileID, desired, updatedBy)
	}
	if err := rows.Err(); err != nil {
		o.cfg.Logger.Error("orchestrator: scan iter", "err", err)
	}
}

func (o *Orchestrator) startJobForBinding(ctx context.Context, muuid, imageID, profileID, desired, bindingActor string) {
	logger := o.cfg.Logger.With("machine_uuid", muuid)

	job, err := o.cfg.Store.Create(ctx, CreateInput{
		MachineUUID: muuid,
		Type:        desired,
		ImageID:     imageID,
		ProfileID:   profileID,
		CreatedBy:   o.cfg.BMCActor,
	})
	if err != nil {
		if errors.Is(err, ErrInFlight) {
			// Another tick created it between SELECT and INSERT; harmless.
			return
		}
		logger.Error("orchestrator: create job", "err", err)
		return
	}
	logger = logger.With("job_id", job.ID)

	// If we have no BMC/IPMI wiring yet, leave the job pending — the agent
	// might still poll for it via M2.3-6 ("manual reboot" path).
	if o.cfg.BMC == nil || o.cfg.IPMI == nil {
		logger.Info("orchestrator: job created, no BMC/IPMI configured — leaving pending")
		_ = o.cfg.Store.AppendLog(ctx, job.ID, "info", "job created; awaiting agent (no IPMI configured)")
		return
	}

	cred, err := o.cfg.BMC.GetWithPassword(ctx, muuid)
	if err != nil {
		_ = o.cfg.Store.AppendLog(ctx, job.ID, "error", fmt.Sprintf("BMC credentials missing: %v", err))
		if failErr := o.cfg.Store.Fail(ctx, job.ID, "BMC credentials missing: "+err.Error()); failErr != nil {
			logger.Error("orchestrator: fail job", "err", failErr)
		}
		return
	}

	_ = o.cfg.Store.AppendLog(ctx, job.ID, "info", "issuing IPMI bootdev=pxe + power cycle")
	if err := o.cfg.IPMI.BootForPXE(ctx, cred); err != nil {
		_ = o.cfg.Store.AppendLog(ctx, job.ID, "error", "ipmi reboot failed: "+sanitize(err.Error(), cred.Password))
		if failErr := o.cfg.Store.Fail(ctx, job.ID, "ipmi reboot failed: "+err.Error()); failErr != nil {
			logger.Error("orchestrator: fail job", "err", failErr)
		}
		return
	}

	// Transition to running and tag the stage so the UI shows movement.
	if _, err := o.cfg.Store.Claim(ctx, job.ID); err != nil {
		// Could be the agent beat us to it via M2.3-6 — log and continue.
		logger.Warn("orchestrator: claim after pxe", "err", err)
	}
	if err := o.cfg.Store.UpdateStage(ctx, job.ID, "pxe_booting"); err != nil {
		logger.Warn("orchestrator: stage update", "err", err)
	}
	_ = o.cfg.Store.AppendLog(ctx, job.ID, "info", "PXE boot initiated; waiting for agent")
	logger.Info("orchestrator: started job")
}

// handleSucceededJobs walks jobs that succeeded but whose binding is still
// pointing at install/reinstall — meaning we haven't yet locked the BMC's
// next-boot to disk and cleared the desired_state. This is idempotent: a
// subsequent tick that finds desired_state=none skips the row.
//
// We filter by `j.finished_at >= b.updated_at` so that **stale** succeeded
// jobs from a *previous* binding revision don't re-fire `bootdev=disk +
// power cycle` when the operator re-arms the binding (e.g. flips desired
// from `none` back to `install`). Without this guard, the orchestrator would
// in the same tick (a) create a new pending job + IPMI bootdev=pxe, then
// (b) re-finalize the old succeeded job with IPMI bootdev=disk —
// the two commands race at the BMC and the machine often ends up booting
// from disk instead of PXE, breaking the reinstall.
func (o *Orchestrator) handleSucceededJobs(ctx context.Context) {
	if o.cfg.BMC == nil || o.cfg.IPMI == nil || o.cfg.Bindings == nil {
		return
	}
	rows, err := o.db.QueryContext(ctx, `
        SELECT j.id, j.machine_uuid
        FROM jobs j
        JOIN bindings b ON b.machine_uuid = j.machine_uuid
        WHERE j.status = 'succeeded'
          AND b.desired_state IN ('install', 'reinstall')
          AND j.finished_at IS NOT NULL
          AND j.finished_at >= b.updated_at
        ORDER BY j.finished_at DESC`)
	if err != nil {
		o.cfg.Logger.Error("orchestrator: scan succeeded", "err", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var jobID, muuid string
		if err := rows.Scan(&jobID, &muuid); err != nil {
			o.cfg.Logger.Error("orchestrator: scan succeeded row", "err", err)
			continue
		}
		o.finalizeMachine(ctx, jobID, muuid)
	}
}

func (o *Orchestrator) finalizeMachine(ctx context.Context, jobID, muuid string) {
	logger := o.cfg.Logger.With("machine_uuid", muuid, "job_id", jobID)
	cred, err := o.cfg.BMC.GetWithPassword(ctx, muuid)
	if err != nil {
		logger.Warn("orchestrator: finalize fetch BMC", "err", err)
		return
	}
	if err := o.cfg.IPMI.FinalizeBootDisk(ctx, cred); err != nil {
		logger.Warn("orchestrator: finalize bootdev=disk", "err", err)
		_ = o.cfg.Store.AppendLog(ctx, jobID, "warn", "post-install bootdev=disk failed: "+sanitize(err.Error(), cred.Password))
		return
	}
	if err := o.cfg.Bindings.ClearDesiredState(ctx, muuid, o.cfg.BMCActor); err != nil {
		logger.Warn("orchestrator: clear desired_state", "err", err)
		return
	}
	_ = o.cfg.Store.AppendLog(ctx, jobID, "info", "post-install bootdev=disk + power cycle issued; binding cleared")
	logger.Info("orchestrator: finalized")
}

// sanitize never emits the password in surfaced log messages.
func sanitize(msg, secret string) string {
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "***")
}
