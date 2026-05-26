package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBMC returns a canned credential or error.
type fakeBMC struct {
	creds map[string]BMCCredential
	err   error
}

func (f *fakeBMC) GetWithPassword(_ context.Context, m string) (BMCCredential, error) {
	if f.err != nil {
		return BMCCredential{}, f.err
	}
	c, ok := f.creds[m]
	if !ok {
		return BMCCredential{}, errors.New("not found")
	}
	return c, nil
}

// fakeIPMI records calls and lets tests inject failures.
type fakeIPMI struct {
	mu             sync.Mutex
	bootForPXE     []string // machine_uuid+ip per call
	finalize       []string
	bootForPXEErr  error
	finalizeErr    error
}

func (f *fakeIPMI) BootForPXE(_ context.Context, cred BMCCredential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bootForPXE = append(f.bootForPXE, cred.IP)
	return f.bootForPXEErr
}

func (f *fakeIPMI) FinalizeBootDisk(_ context.Context, cred BMCCredential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalize = append(f.finalize, cred.IP)
	return f.finalizeErr
}

// fakeBindings tracks ClearDesiredState calls.
type fakeBindings struct {
	mu      sync.Mutex
	cleared []string
}

func (b *fakeBindings) ClearDesiredState(_ context.Context, muuid, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cleared = append(b.cleared, muuid)
	return nil
}

// orchFixture wraps fixture with the orchestrator + collaborators set up.
type orchFixture struct {
	*fixture
	bmc  *fakeBMC
	ipmi *fakeIPMI
	bind *fakeBindings
	orch *Orchestrator
}

func newOrchFixture(t *testing.T) *orchFixture {
	f := newFixture(t)
	// Stub a bindings table the orchestrator can scan against (it doesn't go
	// through the bindings.Store — it queries the same DB directly).
	if _, err := f.db.ExecContext(context.Background(), `
        CREATE TABLE bindings (
            machine_uuid    TEXT PRIMARY KEY,
            image_id        TEXT NOT NULL,
            profile_id      TEXT NOT NULL,
            desired_state   TEXT NOT NULL,
            static_address  TEXT,
            hostname        TEXT,
            updated_at      INTEGER NOT NULL,
            updated_by      TEXT NOT NULL
        )`); err != nil {
		t.Fatalf("bindings stub: %v", err)
	}
	bmc := &fakeBMC{creds: map[string]BMCCredential{}}
	ipmi := &fakeIPMI{}
	bind := &fakeBindings{}
	o, err := NewOrchestrator(OrchestratorConfig{
		Store:        f.store,
		BMC:          bmc,
		IPMI:         ipmi,
		Bindings:     bind,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		TickInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	return &orchFixture{fixture: f, bmc: bmc, ipmi: ipmi, bind: bind, orch: o}
}

func (f *orchFixture) seedBinding(t *testing.T, muuid, imageID, profileID, desired string) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO bindings (machine_uuid, image_id, profile_id, desired_state, updated_at, updated_by)
         VALUES (?, ?, ?, ?, ?, 'admin')`,
		muuid, imageID, profileID, desired, time.Now().Unix()); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
}

func TestOrchestratorTickCreatesJobAndPXEReboots(t *testing.T) {
	f := newOrchFixture(t)
	muuid := f.seedMachine(t, 1)
	imageID := f.seedImage(t)
	profileID := f.seedProfile(t)
	f.seedBinding(t, muuid, imageID, profileID, "install")
	f.bmc.creds[muuid] = BMCCredential{IP: "10.0.0.7", Username: "ADMIN", Password: "p", IPMIInterface: "lanplus", Port: 623}

	f.orch.tick(context.Background())

	if got := len(f.ipmi.bootForPXE); got != 1 {
		t.Fatalf("BootForPXE called %d times, want 1", got)
	}
	if f.ipmi.bootForPXE[0] != "10.0.0.7" {
		t.Errorf("BootForPXE ip=%q", f.ipmi.bootForPXE[0])
	}
	j, err := f.store.CurrentForMachine(context.Background(), muuid)
	if err != nil {
		t.Fatalf("CurrentForMachine: %v", err)
	}
	if j.Status != "running" {
		t.Errorf("status=%q want running", j.Status)
	}
	if j.Stage != "pxe_booting" {
		t.Errorf("stage=%q", j.Stage)
	}
}

func TestOrchestratorTickIdempotent(t *testing.T) {
	f := newOrchFixture(t)
	muuid := f.seedMachine(t, 2)
	imageID := f.seedImage(t)
	profileID := f.seedProfile(t)
	f.seedBinding(t, muuid, imageID, profileID, "install")
	f.bmc.creds[muuid] = BMCCredential{IP: "10.0.0.8", Password: "p"}

	f.orch.tick(context.Background())
	f.orch.tick(context.Background())
	f.orch.tick(context.Background())

	if got := len(f.ipmi.bootForPXE); got != 1 {
		t.Errorf("ticks created %d BootForPXE calls, want 1", got)
	}
}

// TestOrchestratorFailedJobDoesNotRetry locks in the fix for the PXE-reboot
// loop bug: when an install job has already failed against the current
// binding revision, subsequent ticks must NOT spin up a fresh job (which
// would IPMI-reboot the machine over and over). The admin has to re-PUT the
// binding to indicate they want another attempt.
func TestOrchestratorFailedJobDoesNotRetry(t *testing.T) {
	f := newOrchFixture(t)
	muuid := f.seedMachine(t, 99)
	imageID := f.seedImage(t)
	profileID := f.seedProfile(t)
	f.seedBinding(t, muuid, imageID, profileID, "install")
	f.bmc.creds[muuid] = BMCCredential{IP: "10.0.0.99", Password: "p"}

	// Tick 1: orchestrator creates the job, reboots BMC.
	f.orch.tick(context.Background())
	if got := len(f.ipmi.bootForPXE); got != 1 {
		t.Fatalf("first tick: BootForPXE=%d want 1", got)
	}
	// Mark the running job failed (simulates the agent reporting a failure
	// mid-install).
	j, err := f.store.CurrentForMachine(context.Background(), muuid)
	if err != nil {
		t.Fatalf("CurrentForMachine: %v", err)
	}
	if err := f.store.Fail(context.Background(), j.ID, "qemu-img blew up"); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	// Tick 2..4: orchestrator MUST NOT issue another reboot.
	f.orch.tick(context.Background())
	f.orch.tick(context.Background())
	f.orch.tick(context.Background())
	if got := len(f.ipmi.bootForPXE); got != 1 {
		t.Fatalf("after fail: BootForPXE=%d want 1 (must not retry)", got)
	}

	// Operator re-PUTs the binding (touches updated_at). Now a retry IS allowed.
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE bindings SET updated_at = ? WHERE machine_uuid = ?`,
		time.Now().Add(time.Second).Unix(), muuid); err != nil {
		t.Fatalf("touch binding: %v", err)
	}
	f.orch.tick(context.Background())
	if got := len(f.ipmi.bootForPXE); got != 2 {
		t.Fatalf("after binding bump: BootForPXE=%d want 2 (admin asked for retry)", got)
	}
}

func TestOrchestratorBMCMissingFailsJob(t *testing.T) {
	f := newOrchFixture(t)
	muuid := f.seedMachine(t, 3)
	imageID := f.seedImage(t)
	profileID := f.seedProfile(t)
	f.seedBinding(t, muuid, imageID, profileID, "install")
	// no creds seeded → fakeBMC returns "not found"

	f.orch.tick(context.Background())

	jobs, _ := f.store.List(context.Background(), ListFilter{MachineUUID: muuid})
	if len(jobs) != 1 {
		t.Fatalf("jobs=%d", len(jobs))
	}
	if jobs[0].Status != "failed" {
		t.Errorf("status=%q want failed", jobs[0].Status)
	}
	if !strings.Contains(jobs[0].Error, "BMC credentials") {
		t.Errorf("err=%q", jobs[0].Error)
	}
	if got := len(f.ipmi.bootForPXE); got != 0 {
		t.Errorf("no BootForPXE expected, got %d", got)
	}
}

func TestOrchestratorIPMIFailFailsJob(t *testing.T) {
	f := newOrchFixture(t)
	muuid := f.seedMachine(t, 4)
	imageID := f.seedImage(t)
	profileID := f.seedProfile(t)
	f.seedBinding(t, muuid, imageID, profileID, "reinstall")
	f.bmc.creds[muuid] = BMCCredential{IP: "10.0.0.9", Password: "secret-xyz"}
	f.ipmi.bootForPXEErr = errors.New("auth failure with secret-xyz visible")

	f.orch.tick(context.Background())

	jobs, _ := f.store.List(context.Background(), ListFilter{MachineUUID: muuid})
	if len(jobs) != 1 || jobs[0].Status != "failed" {
		t.Fatalf("jobs=%+v", jobs)
	}
	// Stored error message may include the password since it came from the
	// ipmi wrapper untouched — that's a different layer's responsibility. But
	// AppendLog calls go through sanitize().
	logs, _ := f.store.Logs(context.Background(), jobs[0].ID, 0, 100)
	for _, l := range logs {
		if strings.Contains(l.Message, "secret-xyz") {
			t.Errorf("log leaks password: %q", l.Message)
		}
	}
}

func TestOrchestratorFinalizesSucceededJob(t *testing.T) {
	f := newOrchFixture(t)
	muuid := f.seedMachine(t, 5)
	imageID := f.seedImage(t)
	profileID := f.seedProfile(t)
	f.seedBinding(t, muuid, imageID, profileID, "install")
	f.bmc.creds[muuid] = BMCCredential{IP: "10.0.0.10", Password: "p"}

	// First tick: create + start.
	f.orch.tick(context.Background())
	j, _ := f.store.CurrentForMachine(context.Background(), muuid)

	// Simulate agent reporting success.
	if err := f.store.Succeed(context.Background(), j.ID); err != nil {
		t.Fatalf("Succeed: %v", err)
	}

	// Second tick: finalize.
	f.orch.tick(context.Background())

	if got := len(f.ipmi.finalize); got != 1 {
		t.Errorf("FinalizeBootDisk called %d, want 1", got)
	}
	if got := len(f.bind.cleared); got != 1 || f.bind.cleared[0] != muuid {
		t.Errorf("ClearDesiredState calls: %v", f.bind.cleared)
	}
}

// TestOrchestratorDoesNotRefinalizeStaleSucceededJob locks in the fix for a
// real-world bug: after a successful install + finalize, the operator re-arms
// the binding (desired_state none → install) to trigger a fresh reinstall.
// On the next tick:
//
//   - handleInstallRequests correctly creates a new pending job and issues
//     BMC bootdev=pxe + power cycle.
//   - handleSucceededJobs USED to also match the OLD succeeded job (because
//     its binding's desired_state is once again install/reinstall) and
//     issued BMC bootdev=disk + power cycle. The two IPMI commands raced
//     at the BMC and the machine ended up booting from disk instead of PXE,
//     silently breaking the reinstall.
//
// Fix: handleSucceededJobs filters succeeded jobs to those whose
// `finished_at >= b.updated_at` — i.e. only jobs belonging to the *current*
// binding revision get finalized.
func TestOrchestratorDoesNotRefinalizeStaleSucceededJob(t *testing.T) {
	f := newOrchFixture(t)
	muuid := f.seedMachine(t, 12)
	imageID := f.seedImage(t)
	profileID := f.seedProfile(t)
	f.seedBinding(t, muuid, imageID, profileID, "install")
	f.bmc.creds[muuid] = BMCCredential{IP: "10.0.0.50", Password: "p"}

	// Tick 1: kick off the first install + claim → running.
	f.orch.tick(context.Background())
	j1, err := f.store.CurrentForMachine(context.Background(), muuid)
	if err != nil {
		t.Fatalf("CurrentForMachine: %v", err)
	}
	// Agent reports success.
	if err := f.store.Succeed(context.Background(), j1.ID); err != nil {
		t.Fatalf("Succeed: %v", err)
	}

	// Tick 2: finalize the just-succeeded job + clear binding desired_state.
	f.orch.tick(context.Background())
	if got := len(f.ipmi.finalize); got != 1 {
		t.Fatalf("after first finalize: finalize count=%d, want 1", got)
	}

	// Operator re-arms the binding. We *update* updated_at so the new
	// revision is timestamped strictly AFTER j1.finished_at. Without this
	// progression in time, the test can't distinguish "stale" from "fresh".
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE bindings SET desired_state = 'install', updated_at = ? WHERE machine_uuid = ?`,
		time.Now().Add(time.Second).Unix(), muuid); err != nil {
		t.Fatalf("re-arm binding: %v", err)
	}

	// Tick 3: new install request should fire BootForPXE once (for the new
	// job), and the stale succeeded job from before must NOT re-fire
	// FinalizeBootDisk. Without the fix, finalize count would jump to 2.
	f.orch.tick(context.Background())

	if got := len(f.ipmi.bootForPXE); got != 2 {
		t.Errorf("BootForPXE count=%d, want 2 (one for j1, one for re-armed install)", got)
	}
	if got := len(f.ipmi.finalize); got != 1 {
		t.Errorf("FinalizeBootDisk count=%d, want 1 (stale succeeded job must not re-finalize)", got)
	}

	// And a new pending/running job should now exist (different ID).
	j2, err := f.store.CurrentForMachine(context.Background(), muuid)
	if err != nil {
		t.Fatalf("CurrentForMachine after re-arm: %v", err)
	}
	if j2.ID == j1.ID {
		t.Fatalf("expected new job after re-arm, still seeing %s", j1.ID)
	}
}

func TestOrchestratorRunStopsOnCtxCancel(t *testing.T) {
	f := newOrchFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.orch.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestNewOrchestratorRequiresStore(t *testing.T) {
	_, err := NewOrchestrator(OrchestratorConfig{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil {
		t.Errorf("nil Store should fail")
	}
}

func TestOrchestratorWithoutBMCLeavesPending(t *testing.T) {
	f := newOrchFixture(t)
	muuid := f.seedMachine(t, 6)
	imageID := f.seedImage(t)
	profileID := f.seedProfile(t)
	f.seedBinding(t, muuid, imageID, profileID, "install")
	// disable BMC/IPMI
	f.orch.cfg.BMC = nil
	f.orch.cfg.IPMI = nil

	f.orch.tick(context.Background())

	j, err := f.store.CurrentForMachine(context.Background(), muuid)
	if err != nil {
		t.Fatalf("CurrentForMachine: %v", err)
	}
	if j.Status != "pending" {
		t.Errorf("status=%q want pending (no IPMI configured)", j.Status)
	}
}
