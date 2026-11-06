package audit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"metalkit/internal/sqlitedb"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{
		Path:   filepath.Join(t.TempDir(), "test.db"),
		Logger: logger,
		NoSync: true,
	})
	if err != nil {
		t.Fatalf("sqlitedb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := NewStore(context.Background(), db, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestRecordAndList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	s.Record(ctx, "alice", "PUT /api/v1/bindings/abc", "abc", "ok", map[string]any{"status": 200})
	s.Record(ctx, "bob", "binding.password_view", "abc", "ok", nil)

	events, err := s.List(ctx, ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len = %d, want 2", len(events))
	}
	// newest first
	if events[0].Actor != "bob" {
		t.Errorf("events[0].actor = %q, want bob (DESC)", events[0].Actor)
	}
	if events[1].Action != "PUT /api/v1/bindings/abc" {
		t.Errorf("events[1].action = %q", events[1].Action)
	}
	var d map[string]any
	if err := json.Unmarshal(events[1].Details, &d); err != nil {
		t.Fatalf("details not JSON: %v", err)
	}
	if d["status"] != float64(200) {
		t.Errorf("details.status = %v", d["status"])
	}
}

func TestListFilterAndClamp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		s.Record(ctx, "alice", "a", "", "ok", nil)
	}
	s.Record(ctx, "bob", "b", "", "ok", nil)

	al, err := s.List(ctx, ListOptions{Actor: "alice"})
	if err != nil || len(al) != 5 {
		t.Fatalf("actor filter: %v %d", err, len(al))
	}
	if got := (ListOptions{Limit: 0}).clamp().Limit; got != 200 {
		t.Errorf("clamp zero limit = %d, want 200", got)
	}
	if got := (ListOptions{Limit: 9999}).clamp().Limit; got != 500 {
		t.Errorf("clamp big limit = %d, want 500", got)
	}
	if got := (ListOptions{Offset: -3}).clamp().Offset; got != 0 {
		t.Errorf("clamp negative offset = %d, want 0", got)
	}
}

func TestRecordBrokenDBDoesNotPanic(t *testing.T) {
	// Record must be fire-and-forget: with the db closed underneath it
	// logs and returns; it must never panic or block the caller.
	s := newTestStore(t)
	_ = s.db.Close()
	s.Record(context.Background(), "x", "y", "", "ok", nil)
}

// TestPruneAndGCLoop: retention pruning removes only out-of-window events
// and the GC loop stops on ctx cancel.
func TestPruneAndGCLoop(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	// One event inside the window, one aged past 180d.
	s.now = func() time.Time { return now }
	s.Record(ctx, "admin", "test.action", "t1", "ok", nil)
	old := now.Add(-DefaultRetention - time.Hour)
	s.now = func() time.Time { return old }
	s.Record(ctx, "admin", "test.action", "t2", "ok", nil)
	s.now = func() time.Time { return now }

	n, err := s.Prune(ctx, 0)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune removed %d events, want 1 (the aged one)", n)
	}
	events, err := s.List(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 1 || events[0].Target != "t1" {
		t.Fatalf("post-prune events = %+v, want only the fresh one", events)
	}

	// The loop exits with the context; a short interval keeps the test fast.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.GCLoop(runCtx, 50*time.Millisecond); close(done) }()
	time.Sleep(120 * time.Millisecond) // let at least one tick fire
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GCLoop did not exit after ctx cancel")
	}
}
