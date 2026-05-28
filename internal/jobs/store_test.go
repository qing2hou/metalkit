package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metalkit/internal/sqlitedb"
)

// fixture wires the jobs store + the upstream tables it FKs to.
type fixture struct {
	db    *sql.DB
	store *Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{Path: path, Logger: logger})
	if err != nil {
		t.Fatalf("sqlitedb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Stub the FK targets (machines, images, profiles). We don't bring those
	// packages in to keep the test focused on jobs behaviour.
	for _, ddl := range []string{
		`CREATE TABLE machines (uuid TEXT PRIMARY KEY)`,
		`CREATE TABLE images   (id TEXT PRIMARY KEY)`,
		`CREATE TABLE profiles (id TEXT PRIMARY KEY)`,
	} {
		if _, err := db.ExecContext(context.Background(), ddl); err != nil {
			t.Fatalf("ddl %q: %v", ddl, err)
		}
	}

	s, err := NewStore(context.Background(), db, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return &fixture{db: db, store: s}
}

func (f *fixture) seedMachine(t *testing.T, n int) string {
	t.Helper()
	if n < 0 || n > 0xff {
		t.Fatalf("seedMachine: n=%d out of [0,255]", n)
	}
	u := fmt.Sprintf("4c4c4544-0058-3210-8053-c5c04f4638%02x", n)
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO machines (uuid) VALUES (?)`, u); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	return u
}

func (f *fixture) seedImage(t *testing.T) string {
	t.Helper()
	id := strings.Repeat("a", 32)
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO images (id) VALUES (?)`, id); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	return id
}

func (f *fixture) seedProfile(t *testing.T) string {
	t.Helper()
	id := strings.Repeat("b", 32)
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO profiles (id) VALUES (?)`, id); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	return id
}

func (f *fixture) baseInput(t *testing.T, n int) CreateInput {
	return CreateInput{
		MachineUUID: f.seedMachine(t, n),
		Type:        "install",
		ImageID:     f.seedImage(t),
		ProfileID:   f.seedProfile(t),
		CreatedBy:   "admin",
	}
}

// ---- Create ----

func TestCreateHappy(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '0')
	j, err := f.store.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if j.Status != "pending" || j.Stage != "" || j.StartedAt != nil || j.FinishedAt != nil {
		t.Errorf("fresh job state: %+v", j)
	}
	if len(j.ID) != 32 {
		t.Errorf("id len %d", len(j.ID))
	}
}

func TestCreateInFlight(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '1')
	if _, err := f.store.Create(context.Background(), in); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := f.store.Create(context.Background(), in)
	if !errors.Is(err, ErrInFlight) {
		t.Fatalf("second Create: want ErrInFlight, got %v", err)
	}
}

func TestCreateAfterTerminalAllowed(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '2')
	j, err := f.store.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// fail the first job, then a new one for the same machine should be allowed
	if _, err := f.store.Claim(context.Background(), j.ID); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := f.store.Fail(context.Background(), j.ID, "boom"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if _, err := f.store.Create(context.Background(), in); err != nil {
		t.Fatalf("retry Create: %v", err)
	}
}

func TestCreateUnknownMachine(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.Create(context.Background(), CreateInput{
		MachineUUID: "4c4c4544-0058-3210-8053-c5c04f463899",
		Type:        "install",
		ImageID:     f.seedImage(t), ProfileID: f.seedProfile(t),
		CreatedBy: "admin",
	})
	if !errors.Is(err, ErrMachineUnknown) {
		t.Errorf("want ErrMachineUnknown, got %v", err)
	}
}

func TestCreateUnknownImage(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.Create(context.Background(), CreateInput{
		MachineUUID: f.seedMachine(t, '3'),
		Type:        "install",
		ImageID:     strings.Repeat("9", 32), ProfileID: f.seedProfile(t),
		CreatedBy: "admin",
	})
	if !errors.Is(err, ErrImageUnknown) {
		t.Errorf("want ErrImageUnknown, got %v", err)
	}
}

func TestCreateUnknownProfile(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.Create(context.Background(), CreateInput{
		MachineUUID: f.seedMachine(t, '4'),
		Type:        "install",
		ImageID:     f.seedImage(t), ProfileID: strings.Repeat("9", 32),
		CreatedBy: "admin",
	})
	if !errors.Is(err, ErrProfileUnknown) {
		t.Errorf("want ErrProfileUnknown, got %v", err)
	}
}

func TestCreateBadType(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '5')
	in.Type = "nuke"
	_, err := f.store.Create(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "type") {
		t.Errorf("err=%v want type error", err)
	}
}

func TestCreateMissingCreatedBy(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '6')
	in.CreatedBy = ""
	_, err := f.store.Create(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "created_by") {
		t.Errorf("err=%v want created_by error", err)
	}
}

func TestCreateRetryOf(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '7')
	orig, err := f.store.Create(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	// fail original
	if _, err := f.store.Claim(context.Background(), orig.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Fail(context.Background(), orig.ID, "x"); err != nil {
		t.Fatal(err)
	}
	in.RetryOfJobID = orig.ID
	retry, err := f.store.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("retry Create: %v", err)
	}
	if retry.RetryOfJobID != orig.ID {
		t.Errorf("retry_of=%q want %q", retry.RetryOfJobID, orig.ID)
	}
}

func TestCreateRetryOfUnknown(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '8')
	in.RetryOfJobID = strings.Repeat("c", 32)
	_, err := f.store.Create(context.Background(), in)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// ---- state machine ----

func TestClaimPendingToRunning(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '9')
	j, _ := f.store.Create(context.Background(), in)
	got, err := f.store.Claim(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got.Status != "running" || got.StartedAt == nil {
		t.Errorf("after Claim: %+v", got)
	}
}

func TestClaimTwiceRejected(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, 'a')
	j, _ := f.store.Create(context.Background(), in)
	if _, err := f.store.Claim(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.Claim(context.Background(), j.ID)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("want ErrInvalidTransition, got %v", err)
	}
}

func TestClaimNotFound(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.Claim(context.Background(), strings.Repeat("d", 32))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestUpdateStage(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, 'b')
	j, _ := f.store.Create(context.Background(), in)
	_, _ = f.store.Claim(context.Background(), j.ID)
	if err := f.store.UpdateStage(context.Background(), j.ID, "download"); err != nil {
		t.Fatalf("UpdateStage: %v", err)
	}
	got, _ := f.store.Get(context.Background(), j.ID)
	if got.Stage != "download" {
		t.Errorf("stage=%q", got.Stage)
	}
}

func TestUpdateStageOnlyWhenRunning(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, 'c')
	j, _ := f.store.Create(context.Background(), in)
	// pending, not running
	if err := f.store.UpdateStage(context.Background(), j.ID, "x"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("want ErrInvalidTransition, got %v", err)
	}
}

func TestSucceed(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, 'd')
	j, _ := f.store.Create(context.Background(), in)
	_, _ = f.store.Claim(context.Background(), j.ID)
	if err := f.store.Succeed(context.Background(), j.ID); err != nil {
		t.Fatalf("Succeed: %v", err)
	}
	got, _ := f.store.Get(context.Background(), j.ID)
	if got.Status != "succeeded" || got.FinishedAt == nil {
		t.Errorf("after Succeed: %+v", got)
	}
}

func TestSucceedFromPendingRejected(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, 'e')
	j, _ := f.store.Create(context.Background(), in)
	if err := f.store.Succeed(context.Background(), j.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("want ErrInvalidTransition, got %v", err)
	}
}

func TestFailFromPending(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, 'f')
	j, _ := f.store.Create(context.Background(), in)
	if err := f.store.Fail(context.Background(), j.ID, "BMC unreachable"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	got, _ := f.store.Get(context.Background(), j.ID)
	if got.Status != "failed" || !strings.Contains(got.Error, "BMC") {
		t.Errorf("after Fail: %+v", got)
	}
}

func TestCancel(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '_')
	j, _ := f.store.Create(context.Background(), in)
	if err := f.store.Cancel(context.Background(), j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got, _ := f.store.Get(context.Background(), j.ID)
	if got.Status != "cancelled" {
		t.Errorf("status=%q", got.Status)
	}
}

func TestCancelTerminalRejected(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '-')
	j, _ := f.store.Create(context.Background(), in)
	_, _ = f.store.Claim(context.Background(), j.ID)
	_ = f.store.Succeed(context.Background(), j.ID)
	if err := f.store.Cancel(context.Background(), j.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("want ErrInvalidTransition for terminal, got %v", err)
	}
}

func TestDeleteTerminalAlsoRemovesLogs(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '*')
	j, _ := f.store.Create(context.Background(), in)
	_, _ = f.store.Claim(context.Background(), j.ID)
	if err := f.store.AppendLog(context.Background(), j.ID, "info", "hello"); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
	_ = f.store.Succeed(context.Background(), j.ID)
	if err := f.store.Delete(context.Background(), j.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.store.Get(context.Background(), j.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: want ErrNotFound, got %v", err)
	}
	logs, err := f.store.Logs(context.Background(), j.ID, 0, 100)
	if err != nil {
		t.Fatalf("Logs after Delete: %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("logs after Delete: got %d, want 0", len(logs))
	}
}

func TestDeletePendingRejected(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '?')
	j, _ := f.store.Create(context.Background(), in)
	if err := f.store.Delete(context.Background(), j.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("Delete pending: want ErrInvalidTransition, got %v", err)
	}
}

func TestDeleteRunningRejected(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '#')
	j, _ := f.store.Create(context.Background(), in)
	_, _ = f.store.Claim(context.Background(), j.ID)
	if err := f.store.Delete(context.Background(), j.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("Delete running: want ErrInvalidTransition, got %v", err)
	}
}

func TestDeleteAllTerminal(t *testing.T) {
	f := newFixture(t)

	// Seed: 1 succeeded, 1 failed, 1 cancelled, 1 pending, 1 running.
	makeTerminal := func(n int, finisher func(string) error) string {
		in := f.baseInput(t, n)
		j, _ := f.store.Create(context.Background(), in)
		_, _ = f.store.Claim(context.Background(), j.ID)
		_ = f.store.AppendLog(context.Background(), j.ID, "info", "x")
		if err := finisher(j.ID); err != nil {
			t.Fatalf("finisher: %v", err)
		}
		return j.ID
	}
	makeTerminal('1', func(id string) error { return f.store.Succeed(context.Background(), id) })
	makeTerminal('2', func(id string) error { return f.store.Fail(context.Background(), id, "boom") })
	// cancel goes from pending, so a slightly different path:
	inC := f.baseInput(t, '3')
	jC, _ := f.store.Create(context.Background(), inC)
	_ = f.store.Cancel(context.Background(), jC.ID)

	// Pending job — survives.
	inP := f.baseInput(t, '4')
	jP, _ := f.store.Create(context.Background(), inP)

	// Running job — survives.
	inR := f.baseInput(t, '5')
	jR, _ := f.store.Create(context.Background(), inR)
	_, _ = f.store.Claim(context.Background(), jR.ID)
	_ = f.store.AppendLog(context.Background(), jR.ID, "info", "still going")

	n, err := f.store.DeleteAllTerminal(context.Background())
	if err != nil {
		t.Fatalf("DeleteAllTerminal: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted=%d want 3", n)
	}
	// Pending + running remain.
	all, _ := f.store.List(context.Background(), ListFilter{})
	if len(all) != 2 {
		t.Errorf("remaining=%d want 2", len(all))
	}
	for _, j := range all {
		if j.Status != "pending" && j.Status != "running" {
			t.Errorf("survivor status=%s", j.Status)
		}
	}
	// Running job's logs preserved.
	logs, _ := f.store.Logs(context.Background(), jR.ID, 0, 100)
	if len(logs) != 1 {
		t.Errorf("running logs=%d want 1", len(logs))
	}
	// And pending stayed reachable.
	if _, err := f.store.Get(context.Background(), jP.ID); err != nil {
		t.Errorf("pending Get: %v", err)
	}
}

func TestDeleteAllTerminalEmpty(t *testing.T) {
	f := newFixture(t)
	n, err := f.store.DeleteAllTerminal(context.Background())
	if err != nil {
		t.Fatalf("DeleteAllTerminal empty: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted=%d want 0", n)
	}
}

func TestDeleteNotFound(t *testing.T) {
	f := newFixture(t)
	bogus := strings.Repeat("a", 32)
	if err := f.store.Delete(context.Background(), bogus); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete nonexistent: want ErrNotFound, got %v", err)
	}
}

func TestErrorMessageTruncated(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '~')
	j, _ := f.store.Create(context.Background(), in)
	_, _ = f.store.Claim(context.Background(), j.ID)
	huge := strings.Repeat("x", MaxErrorLen*2)
	if err := f.store.Fail(context.Background(), j.ID, huge); err != nil {
		t.Fatal(err)
	}
	got, _ := f.store.Get(context.Background(), j.ID)
	if len(got.Error) != MaxErrorLen {
		t.Errorf("err len=%d want %d", len(got.Error), MaxErrorLen)
	}
}

// ---- queries ----

func TestCurrentForMachine(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '!')
	j, _ := f.store.Create(context.Background(), in)
	got, err := f.store.CurrentForMachine(context.Background(), in.MachineUUID)
	if err != nil {
		t.Fatalf("CurrentForMachine: %v", err)
	}
	if got.ID != j.ID {
		t.Errorf("id mismatch")
	}
	// terminal → no current
	_, _ = f.store.Claim(context.Background(), j.ID)
	_ = f.store.Succeed(context.Background(), j.ID)
	if _, err := f.store.CurrentForMachine(context.Background(), in.MachineUUID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after succeed: want ErrNotFound, got %v", err)
	}
}

func TestListByMachineAndStatus(t *testing.T) {
	f := newFixture(t)
	in1 := f.baseInput(t, '@')
	in2 := f.baseInput(t, '#')
	j1, _ := f.store.Create(context.Background(), in1)
	_, _ = f.store.Create(context.Background(), in2)
	_, _ = f.store.Claim(context.Background(), j1.ID)
	_ = f.store.Fail(context.Background(), j1.ID, "x")

	all, err := f.store.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("all len=%d", len(all))
	}
	pending, _ := f.store.List(context.Background(), ListFilter{Status: "pending"})
	if len(pending) != 1 {
		t.Errorf("pending len=%d", len(pending))
	}
	for1, _ := f.store.List(context.Background(), ListFilter{MachineUUID: in1.MachineUUID})
	if len(for1) != 1 {
		t.Errorf("machine1 len=%d", len(for1))
	}
}

// ---- logs ----

func TestAppendAndStreamLogs(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '$')
	j, _ := f.store.Create(context.Background(), in)
	_, _ = f.store.Claim(context.Background(), j.ID)
	for i, line := range []string{"start", "downloading image", "writing disk", "done"} {
		level := "info"
		if i == 0 {
			level = "debug"
		}
		if err := f.store.AppendLog(context.Background(), j.ID, level, line); err != nil {
			t.Fatalf("AppendLog %d: %v", i, err)
		}
	}
	all, err := f.store.Logs(context.Background(), j.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("logs len=%d", len(all))
	}
	if all[0].Message != "start" || all[0].Level != "debug" {
		t.Errorf("first: %+v", all[0])
	}
	// resume from after the second line
	after, _ := f.store.Logs(context.Background(), j.ID, all[1].ID, 100)
	if len(after) != 2 {
		t.Errorf("after id=%d len=%d", all[1].ID, len(after))
	}
}

func TestAppendLogUnknownJob(t *testing.T) {
	f := newFixture(t)
	err := f.store.AppendLog(context.Background(), strings.Repeat("e", 32), "info", "hi")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestAppendLogBadLevel(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '%')
	j, _ := f.store.Create(context.Background(), in)
	if err := f.store.AppendLog(context.Background(), j.ID, "fatal", "x"); err == nil {
		t.Errorf("bad level should fail")
	}
}

func TestLogMessageTruncated(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '^')
	j, _ := f.store.Create(context.Background(), in)
	huge := strings.Repeat("y", MaxLogMessageLen*2)
	_ = f.store.AppendLog(context.Background(), j.ID, "info", huge)
	logs, _ := f.store.Logs(context.Background(), j.ID, 0, 10)
	if len(logs) != 1 || len(logs[0].Message) != MaxLogMessageLen {
		t.Errorf("truncate: %+v", logs)
	}
}

// ---- ref counts ----

func TestRefCountByImage(t *testing.T) {
	f := newFixture(t)
	in := f.baseInput(t, '&')
	j, _ := f.store.Create(context.Background(), in)
	n, _ := f.store.RefCountByImage(context.Background(), in.ImageID)
	if n != 1 {
		t.Errorf("pending count=%d", n)
	}
	_, _ = f.store.Claim(context.Background(), j.ID)
	_ = f.store.Succeed(context.Background(), j.ID)
	n, _ = f.store.RefCountByImage(context.Background(), in.ImageID)
	if n != 0 {
		t.Errorf("after succeed terminal count=%d (should not block delete)", n)
	}
}

// ---- timestamps ----

func TestTimestampsClock(t *testing.T) {
	f := newFixture(t)
	fixed := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	f.store.now = func() time.Time { return fixed }
	in := f.baseInput(t, '+')
	j, _ := f.store.Create(context.Background(), in)
	if !j.CreatedAt.Equal(fixed) {
		t.Errorf("created_at=%v want %v", j.CreatedAt, fixed)
	}
}
