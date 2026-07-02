// Package jobs is the install/reinstall job catalog. A job is the record of
// "we tried to install image X with profile Y onto machine Z" — its state
// machine drives the orchestrator's BMC calls (start a job → ipmitool bootdev
// pxe + power cycle) and serves as the bridge between the operator (PUT
// binding desired_state=install) and the live-boot agent (claims job, streams
// log lines, reports terminal state).
//
// Concurrency model:
//   - Per machine_uuid mutex enforced by a UNIQUE partial index on
//     status ∈ {pending, running}. Two simultaneous Create() calls for the
//     same machine: one wins, the other gets ErrInFlight.
//   - State transitions are guarded by atomic UPDATE … WHERE status = oldState;
//     callers see ErrInvalidTransition if the row isn't in the expected state.
//
// Failure model:
//   - No auto-retry. A failed job stays failed; admin must explicitly create a
//     new job (Create with retry_of_job_id set) to try again.
//   - Cancellation is one-way; cancelled jobs cannot be resumed.
package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

var (
	ErrNotFound           = errors.New("jobs: not found")
	ErrInFlight           = errors.New("jobs: machine already has a pending or running job")
	ErrMachineUnknown     = errors.New("jobs: machine_uuid not in inventory")
	ErrImageUnknown       = errors.New("jobs: image_id not in catalog")
	ErrProfileUnknown     = errors.New("jobs: profile_id not in catalog")
	ErrInvalidTransition  = errors.New("jobs: invalid state transition")
)

// Store reads and writes jobs and job_logs rows.
type Store struct {
	db     *sql.DB
	logger *slog.Logger
	// now is overridable for tests; defaults to time.Now.
	now func() time.Time
}

// NewStore applies the jobs schema. Must run after machines/images/profiles
// so the FK declarations resolve.
func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger) (*Store, error) {
	if db == nil {
		return nil, errors.New("jobs: db is required")
	}
	if logger == nil {
		return nil, errors.New("jobs: logger is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply jobs schema: %w", err)
	}
	return &Store{db: db, logger: logger, now: time.Now}, nil
}

// Job is the JSON record. Times are unix-seconds-or-nil internally; the
// struct exposes them as time.Time / *time.Time for JSON consumers.
type Job struct {
	ID            string     `json:"id"`
	MachineUUID   string     `json:"machine_uuid"`
	Type          string     `json:"type"`
	ImageID       string     `json:"image_id"`
	ProfileID     string     `json:"profile_id"`
	Status        string     `json:"status"`
	Stage         string     `json:"stage,omitempty"`
	Error         string     `json:"error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	CreatedBy     string     `json:"created_by"`
	RetryOfJobID  string     `json:"retry_of_job_id,omitempty"`
}

// CreateInput is what the API / orchestrator passes.
type CreateInput struct {
	MachineUUID  string
	Type         string // install | reinstall
	ImageID      string
	ProfileID    string
	CreatedBy    string
	RetryOfJobID string // optional
}

// Create inserts a new pending job. Returns ErrInFlight if the same machine
// already has a pending or running job (UNIQUE partial index).
func (s *Store) Create(ctx context.Context, in CreateInput) (*Job, error) {
	muuid, err := validateMachineUUID(in.MachineUUID)
	if err != nil {
		return nil, err
	}
	imageID, err := validateID("image_id", in.ImageID)
	if err != nil {
		return nil, err
	}
	profileID, err := validateID("profile_id", in.ProfileID)
	if err != nil {
		return nil, err
	}
	jobType, err := validateType(in.Type)
	if err != nil {
		return nil, err
	}
	if in.CreatedBy == "" {
		return nil, errors.New("jobs: created_by is required")
	}
	var retryOf sql.NullString
	if in.RetryOfJobID != "" {
		id, err := validateJobID(in.RetryOfJobID)
		if err != nil {
			return nil, err
		}
		retryOf = sql.NullString{String: id, Valid: true}
	}

	if err := s.checkMachineExists(ctx, muuid); err != nil {
		return nil, err
	}
	if err := s.checkImageExists(ctx, imageID); err != nil {
		return nil, err
	}
	if err := s.checkProfileExists(ctx, profileID); err != nil {
		return nil, err
	}
	if retryOf.Valid {
		if err := s.checkJobExists(ctx, retryOf.String); err != nil {
			return nil, err
		}
	}

	id, err := newJobID()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Unix()

	_, err = s.db.ExecContext(ctx, `
        INSERT INTO jobs
            (id, machine_uuid, type, image_id, profile_id, status, stage,
             error, created_at, started_at, finished_at, created_by, retry_of_job_id)
        VALUES (?, ?, ?, ?, ?, 'pending', '', NULL, ?, NULL, NULL, ?, ?)`,
		id, muuid, jobType, imageID, profileID, now, in.CreatedBy, retryOf,
	)
	if err != nil {
		if isInFlightConflict(err) {
			return nil, ErrInFlight
		}
		return nil, fmt.Errorf("create job: %w", err)
	}

	return &Job{
		ID:           id,
		MachineUUID:  muuid,
		Type:         jobType,
		ImageID:      imageID,
		ProfileID:    profileID,
		Status:       "pending",
		CreatedAt:    time.Unix(now, 0).UTC(),
		CreatedBy:    in.CreatedBy,
		RetryOfJobID: retryOf.String,
	}, nil
}

// Get returns one job by id.
func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	id, err := validateJobID(id)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, selectJobSQL+` WHERE id = ?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// ListFilter narrows List() to a specific machine, status, or both.
type ListFilter struct {
	MachineUUID string
	Status      string
	Limit       int // 0 → 100
}

// List returns jobs in reverse-chronological order (newest first).
func (s *Store) List(ctx context.Context, f ListFilter) ([]Job, error) {
	q := selectJobSQL
	args := []any{}
	conds := []string{}
	if f.MachineUUID != "" {
		muuid, err := validateMachineUUID(f.MachineUUID)
		if err != nil {
			return nil, err
		}
		conds = append(conds, "machine_uuid = ?")
		args = append(args, muuid)
	}
	if f.Status != "" {
		if !validJobStatuses[f.Status] {
			return nil, fmt.Errorf("status %q: invalid", f.Status)
		}
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY created_at DESC, id DESC"
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	q += " LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	out := make([]Job, 0)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// CurrentForMachine returns the in-flight job (pending|running) for a machine,
// or ErrNotFound. Used by the agent's "give me my job" path (M2.3-6).
func (s *Store) CurrentForMachine(ctx context.Context, machineUUID string) (*Job, error) {
	muuid, err := validateMachineUUID(machineUUID)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		selectJobSQL+` WHERE machine_uuid = ? AND status IN ('pending','running') LIMIT 1`,
		muuid)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// Claim atomically transitions pending → running and stamps started_at. Used
// by agents (or the orchestrator after BMC reboot is in-flight). Returns
// ErrInvalidTransition if the row was not pending.
func (s *Store) Claim(ctx context.Context, id string) (*Job, error) {
	id, err := validateJobID(id)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
        UPDATE jobs
        SET status = 'running', started_at = ?
        WHERE id = ? AND status = 'pending'`,
		now, id)
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, s.classifyMissedUpdate(ctx, id)
	}
	return s.Get(ctx, id)
}

// UpdateStage moves the job's stage marker forward. Allowed only when status=running.
func (s *Store) UpdateStage(ctx context.Context, id, stage string) error {
	id, err := validateJobID(id)
	if err != nil {
		return err
	}
	stage, err = validateStage(stage)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
        UPDATE jobs SET stage = ?
        WHERE id = ? AND status = 'running'`,
		stage, id)
	if err != nil {
		return fmt.Errorf("update stage: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return s.classifyMissedUpdate(ctx, id)
	}
	return nil
}

// Succeed transitions running → succeeded and stamps finished_at.
func (s *Store) Succeed(ctx context.Context, id string) error {
	return s.terminate(ctx, id, "succeeded", "", []string{"running"})
}

// Fail transitions running → failed and stamps finished_at + error.
// Truncates error message at MaxErrorLen.
func (s *Store) Fail(ctx context.Context, id, errMsg string) error {
	msg, _ := validateError(errMsg)
	return s.terminate(ctx, id, "failed", msg, []string{"pending", "running"})
}

// Cancel transitions pending|running → cancelled.
func (s *Store) Cancel(ctx context.Context, id string) error {
	return s.terminate(ctx, id, "cancelled", "", []string{"pending", "running"})
}

// DeleteAllTerminal removes every job in a terminal state (succeeded / failed /
// cancelled) plus its log lines, in a single transaction. Returns the count of
// jobs removed. Pending and running jobs are untouched — the orchestrator and
// agent's state machine assumptions stay valid.
func (s *Store) DeleteAllTerminal(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
        DELETE FROM job_logs
        WHERE job_id IN (
            SELECT id FROM jobs WHERE status IN ('succeeded','failed','cancelled')
        )`); err != nil {
		return 0, fmt.Errorf("delete logs: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM jobs WHERE status IN ('succeeded','failed','cancelled')`)
	if err != nil {
		return 0, fmt.Errorf("delete jobs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Delete removes a terminal-state job and its log lines. Pending/running jobs
// are rejected with ErrInvalidTransition — the caller must Cancel first so the
// orchestrator state machine stays consistent. ErrNotFound if no such id.
func (s *Store) Delete(ctx context.Context, id string) error {
	id, err := validateJobID(id)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup: %w", err)
	}
	if status != "succeeded" && status != "failed" && status != "cancelled" {
		return fmt.Errorf("%w: status=%s (cancel first)", ErrInvalidTransition, status)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM job_logs WHERE job_id = ?`, id); err != nil {
		return fmt.Errorf("delete logs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete job: %w", err)
	}
	return tx.Commit()
}

// DeleteByImage removes every job referencing the given image id. job_logs
// cascade-delete via their ON DELETE CASCADE FK. Used by the images delete
// handler so images can be removed even when historical jobs reference them.
func (s *Store) DeleteByImage(ctx context.Context, imageID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE image_id = ?`, imageID)
	if err != nil {
		return 0, fmt.Errorf("delete jobs by image: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) terminate(ctx context.Context, id, target, errMsg string, allowedFrom []string) error {
	id, err := validateJobID(id)
	if err != nil {
		return err
	}
	now := s.now().UTC().Unix()
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(allowedFrom)), ",")
	args := make([]any, 0, len(allowedFrom)+3)
	var errSQL any
	if errMsg == "" {
		errSQL = nil
	} else {
		errSQL = errMsg
	}
	args = append(args, target, errSQL, now, id)
	for _, st := range allowedFrom {
		args = append(args, st)
	}
	q := `UPDATE jobs SET status = ?, error = ?, finished_at = ?
          WHERE id = ? AND status IN (` + placeholders + `)`
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("terminate job: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return s.classifyMissedUpdate(ctx, id)
	}
	return nil
}

// classifyMissedUpdate disambiguates "row doesn't exist" from "row exists but
// is in a state we couldn't transition from". Called after RowsAffected=0 on
// a guarded UPDATE.
func (s *Store) classifyMissedUpdate(ctx context.Context, id string) error {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("classify update: %w", err)
	}
	return fmt.Errorf("%w: status=%s", ErrInvalidTransition, status)
}

// AppendLog writes one log line. Returns ErrNotFound if the job doesn't exist.
// Truncates the message at MaxLogMessageLen.
func (s *Store) AppendLog(ctx context.Context, jobID, level, message string) error {
	jobID, err := validateJobID(jobID)
	if err != nil {
		return err
	}
	level, err = validateLogLevel(level)
	if err != nil {
		return err
	}
	message, _ = validateLogMessage(message)
	now := s.now().UTC().Unix()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO job_logs (job_id, ts, level, message) VALUES (?, ?, ?, ?)`,
		jobID, now, level, message)
	if err != nil {
		// modernc.org/sqlite surfaces FK violation as "FOREIGN KEY constraint failed"
		// only when PRAGMA foreign_keys=ON. Fall back to explicit existence probe.
		if existsErr := s.checkJobExists(ctx, jobID); existsErr != nil {
			return existsErr
		}
		return fmt.Errorf("append log: %w", err)
	}
	return nil
}

// JobLog is a single log line.
type JobLog struct {
	ID      int64     `json:"id"`
	JobID   string    `json:"job_id"`
	Ts      time.Time `json:"ts"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// Logs returns lines for a job whose id > sinceID, in stream order. sinceID=0
// means "from the beginning". The UI's SSE tailer calls this in a loop with
// the last-seen ID.
func (s *Store) Logs(ctx context.Context, jobID string, sinceID int64, limit int) ([]JobLog, error) {
	jobID, err := validateJobID(jobID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, job_id, ts, level, message
        FROM job_logs
        WHERE job_id = ? AND id > ?
        ORDER BY id ASC
        LIMIT ?`,
		jobID, sinceID, limit)
	if err != nil {
		return nil, fmt.Errorf("query logs: %w", err)
	}
	defer rows.Close()
	out := make([]JobLog, 0)
	for rows.Next() {
		var l JobLog
		var ts int64
		if err := rows.Scan(&l.ID, &l.JobID, &ts, &l.Level, &l.Message); err != nil {
			return nil, fmt.Errorf("scan log: %w", err)
		}
		l.Ts = time.Unix(ts, 0).UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---- internal helpers ----

const selectJobSQL = `SELECT id, machine_uuid, type, image_id, profile_id,
       status, stage, error, created_at, started_at, finished_at,
       created_by, retry_of_job_id FROM jobs`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(r rowScanner) (*Job, error) {
	var j Job
	var stage, errStr, retryOf sql.NullString
	var startedAt, finishedAt sql.NullInt64
	var createdAt int64
	if err := r.Scan(
		&j.ID, &j.MachineUUID, &j.Type, &j.ImageID, &j.ProfileID,
		&j.Status, &stage, &errStr, &createdAt, &startedAt, &finishedAt,
		&j.CreatedBy, &retryOf,
	); err != nil {
		return nil, err
	}
	j.Stage = stage.String
	j.Error = errStr.String
	j.RetryOfJobID = retryOf.String
	j.CreatedAt = time.Unix(createdAt, 0).UTC()
	if startedAt.Valid {
		t := time.Unix(startedAt.Int64, 0).UTC()
		j.StartedAt = &t
	}
	if finishedAt.Valid {
		t := time.Unix(finishedAt.Int64, 0).UTC()
		j.FinishedAt = &t
	}
	return &j, nil
}

func (s *Store) checkMachineExists(ctx context.Context, uuid string) error {
	var seen int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM machines WHERE uuid = ?`, uuid).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrMachineUnknown, uuid)
	}
	if err != nil {
		return fmt.Errorf("check machine: %w", err)
	}
	return nil
}

func (s *Store) checkImageExists(ctx context.Context, id string) error {
	var seen int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM images WHERE id = ?`, id).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrImageUnknown, id)
	}
	if err != nil {
		return fmt.Errorf("check image: %w", err)
	}
	return nil
}

func (s *Store) checkProfileExists(ctx context.Context, id string) error {
	var seen int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM profiles WHERE id = ?`, id).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrProfileUnknown, id)
	}
	if err != nil {
		return fmt.Errorf("check profile: %w", err)
	}
	return nil
}

func (s *Store) checkJobExists(ctx context.Context, id string) error {
	var seen int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id = ?`, id).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("check job: %w", err)
	}
	return nil
}

// RefCountByImage / RefCountByProfile let images/profiles delete handlers
// refuse the delete if jobs still reference them. Counts only non-terminal
// jobs; succeeded/failed/cancelled jobs are historical and shouldn't block
// catalog cleanup forever.
func (s *Store) RefCountByImage(ctx context.Context, imageID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE image_id = ? AND status IN ('pending','running')`,
		imageID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count jobs for image: %w", err)
	}
	return n, nil
}

func (s *Store) RefCountByProfile(ctx context.Context, profileID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE profile_id = ? AND status IN ('pending','running')`,
		profileID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count jobs for profile: %w", err)
	}
	return n, nil
}

// DeleteByProfile removes every job referencing the given profile id.
// job_logs cascade-delete via their ON DELETE CASCADE FK. Used by the
// profiles delete handler so profiles can be removed even when historical
// jobs reference them.
func (s *Store) DeleteByProfile(ctx context.Context, profileID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE profile_id = ?`, profileID)
	if err != nil {
		return 0, fmt.Errorf("delete jobs by profile: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func newJobID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// isInFlightConflict matches modernc.org/sqlite's error message for the
// UNIQUE partial index on (machine_uuid, status ∈ pending/running).
// modernc surfaces the violating column ("jobs.machine_uuid") rather than the
// index name, so we match on the column path.
func isInFlightConflict(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") &&
		strings.Contains(s, "jobs.machine_uuid")
}
