package sessions_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"metalkit/internal/sessions"
	"metalkit/internal/sqlitedb"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{
		Path:   ":memory:",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newStore is the standard constructor for tests that don't need a fake clock.
func newStore(t *testing.T) *sessions.Store {
	t.Helper()
	db := openTestDB(t)
	s, err := sessions.NewStore(context.Background(), db, discardLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// newStoreWithClock builds a Store and overrides its clock via
// sessions.SetClockForTest (defined in store_export_test.go).
func newStoreWithClock(t *testing.T, clk *fakeClock) *sessions.Store {
	t.Helper()
	db := openTestDB(t)
	s, err := sessions.NewStore(context.Background(), db, discardLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	sessions.SetClockForTest(s, clk.Now)
	return s
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time            { return c.now }
func (c *fakeClock) Advance(d time.Duration)   { c.now = c.now.Add(d) }
func (c *fakeClock) Set(t time.Time)           { c.now = t }

func TestNewStoreIdempotent(t *testing.T) {
	db := openTestDB(t)
	if _, err := sessions.NewStore(context.Background(), db, discardLogger()); err != nil {
		t.Fatalf("first NewStore: %v", err)
	}
	if _, err := sessions.NewStore(context.Background(), db, discardLogger()); err != nil {
		t.Fatalf("second NewStore: %v", err)
	}
}

func TestNewStoreRequiresDBAndLogger(t *testing.T) {
	if _, err := sessions.NewStore(context.Background(), nil, discardLogger()); err == nil {
		t.Fatalf("expected error on nil db")
	}
	db := openTestDB(t)
	if _, err := sessions.NewStore(context.Background(), db, nil); err == nil {
		t.Fatalf("expected error on nil logger")
	}
}

func TestCreateGeneratesHexIDs(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		sess, err := s.Create(ctx, "alice", time.Hour)
		if err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
		if len(sess.ID) != 64 {
			t.Fatalf("ID len=%d, want 64", len(sess.ID))
		}
		for j := 0; j < len(sess.ID); j++ {
			c := sess.ID[j]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Fatalf("ID has non-lowercase-hex char %q at %d", c, j)
			}
		}
		if seen[sess.ID] {
			t.Fatalf("collision at #%d: %s", i, sess.ID)
		}
		seen[sess.ID] = true
	}
}

func TestCreateRejectsBadUsername(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		u    string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"tab only", "\t"},
		{"too long", strings.Repeat("a", 65)},
		{"contains NUL", "ali\x00ce"},
		{"contains newline mid-string", "ali\nce"},
		{"contains DEL", "ali\x7fce"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Create(ctx, tc.u, time.Hour); err == nil {
				t.Fatalf("expected error for %q", tc.u)
			}
		})
	}
}

func TestCreateRejectsBadTTL(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, "alice", 0); err == nil {
		t.Fatalf("expected error on zero ttl")
	}
	if _, err := s.Create(ctx, "alice", -time.Second); err == nil {
		t.Fatalf("expected error on negative ttl")
	}
}

func TestCreateAcceptsMaxUsername(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	u := strings.Repeat("a", 64)
	sess, err := s.Create(ctx, u, time.Hour)
	if err != nil {
		t.Fatalf("Create 64-char: %v", err)
	}
	if sess.Username != u {
		t.Fatalf("Username=%q, want %q", sess.Username, u)
	}
}

func TestCreateTrimsUsername(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	sess, err := s.Create(ctx, "  alice  ", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.Username != "alice" {
		t.Fatalf("Username=%q, want %q", sess.Username, "alice")
	}
}

func TestGetRoundTrip(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)}
	s := newStoreWithClock(t, clk)
	ctx := context.Background()

	created, err := s.Create(ctx, "alice", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != created.ID || got.Username != "alice" {
		t.Fatalf("Get returned %+v, want id=%s user=alice", got, created.ID)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt location = %v, want UTC", got.CreatedAt.Location())
	}
	if got.LastSeenAt.Location() != time.UTC {
		t.Fatalf("LastSeenAt location = %v, want UTC", got.LastSeenAt.Location())
	}
	if got.ExpiresAt.Location() != time.UTC {
		t.Fatalf("ExpiresAt location = %v, want UTC", got.ExpiresAt.Location())
	}
	if !got.CreatedAt.Equal(clk.now) {
		t.Fatalf("CreatedAt=%v, want %v", got.CreatedAt, clk.now)
	}
	if !got.ExpiresAt.Equal(clk.now.Add(time.Hour)) {
		t.Fatalf("ExpiresAt=%v, want %v", got.ExpiresAt, clk.now.Add(time.Hour))
	}
}

func TestGetUnknownID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	id := strings.Repeat("a", 64) // valid format, no such row
	_, err := s.Get(ctx, id)
	if !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Get unknown: err=%v, want ErrNotFound", err)
	}
}

func TestGetExpired(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)}
	s := newStoreWithClock(t, clk)
	ctx := context.Background()

	sess, err := s.Create(ctx, "alice", time.Minute)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	clk.Advance(2 * time.Minute) // past expires_at
	_, err = s.Get(ctx, sess.ID)
	if !errors.Is(err, sessions.ErrExpired) {
		t.Fatalf("Get expired: err=%v, want ErrExpired", err)
	}
}

func TestGetInvalidIDIsNotFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	bad := []string{
		"",
		"too-short",
		strings.Repeat("z", 64),                  // non-hex
		strings.Repeat("A", 64),                  // uppercase
		strings.Repeat("a", 63),                  // wrong length
		strings.Repeat("a", 65),                  // wrong length
		strings.Repeat("a", 32) + "; DROP TABLE", // injection-ish
	}
	for _, b := range bad {
		_, err := s.Get(ctx, b)
		if !errors.Is(err, sessions.ErrNotFound) {
			t.Errorf("Get(%q): err=%v, want ErrNotFound", b, err)
		}
	}
}

func TestTouchWithinIntervalIsNoop(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)}
	s := newStoreWithClock(t, clk)
	ctx := context.Background()

	sess, err := s.Create(ctx, "alice", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	originalExpires := sess.ExpiresAt

	// Just five minutes later — well under minInterval of 1h.
	clk.Advance(5 * time.Minute)

	got, err := s.Touch(ctx, sess.ID, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("Touch returned %v, want zero time (no-op)", got)
	}

	// Confirm the row was not actually updated.
	after, err := s.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after no-op Touch: %v", err)
	}
	if !after.ExpiresAt.Equal(originalExpires) {
		t.Fatalf("ExpiresAt drifted after no-op Touch: got %v, want %v", after.ExpiresAt, originalExpires)
	}
}

func TestTouchPastIntervalRenews(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)}
	s := newStoreWithClock(t, clk)
	ctx := context.Background()

	sess, err := s.Create(ctx, "alice", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Past minInterval.
	clk.Advance(90 * time.Minute)
	// But expires_at was 1h after creation, so the session is now expired —
	// for Touch we don't gate on expiry, only on minInterval. We still want
	// Touch to renew. (Whether the HTTP layer accepts a touch on an expired
	// session is Slice B's call.)
	got, err := s.Touch(ctx, sess.ID, time.Hour, 2*time.Hour)
	if err != nil {
		t.Fatalf("Touch: %v", err)
	}
	wantExpires := clk.now.Add(2 * time.Hour)
	if !got.Equal(wantExpires) {
		t.Fatalf("Touch expires = %v, want %v", got, wantExpires)
	}

	// Verify the DB row was updated.
	row, err := s.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after Touch: %v", err)
	}
	if !row.LastSeenAt.Equal(clk.now) {
		t.Fatalf("LastSeenAt = %v, want %v", row.LastSeenAt, clk.now)
	}
	if !row.ExpiresAt.Equal(wantExpires) {
		t.Fatalf("ExpiresAt = %v, want %v", row.ExpiresAt, wantExpires)
	}
}

func TestTouchUnknownID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	id := strings.Repeat("b", 64) // valid format, missing row
	_, err := s.Touch(ctx, id, time.Hour, time.Hour)
	if !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Touch unknown: err=%v, want ErrNotFound", err)
	}
}

func TestTouchInvalidIDIsNotFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, err := s.Touch(ctx, "not-hex", time.Hour, time.Hour)
	if !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Touch invalid: err=%v, want ErrNotFound", err)
	}
}

func TestTouchRejectsBadTTL(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	sess, err := s.Create(ctx, "alice", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Touch(ctx, sess.ID, time.Hour, 0); err == nil {
		t.Fatalf("expected error on zero ttl")
	}
}

func TestDeleteIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	sess, err := s.Create(ctx, "alice", time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	// Second delete is fine.
	if err := s.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	// Get now reports ErrNotFound.
	if _, err := s.Get(ctx, sess.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Get after Delete: err=%v, want ErrNotFound", err)
	}
}

func TestDeleteInvalidIDIsNoError(t *testing.T) {
	s := newStore(t)
	if err := s.Delete(context.Background(), "not-hex"); err != nil {
		t.Fatalf("Delete invalid id: %v", err)
	}
}

func TestDeleteExpired(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)}
	s := newStoreWithClock(t, clk)
	ctx := context.Background()

	// Create 3 sessions with various ttls.
	shortA, err := s.Create(ctx, "alice", 5*time.Minute)
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	shortB, err := s.Create(ctx, "bob", 5*time.Minute)
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}
	long, err := s.Create(ctx, "carol", 24*time.Hour)
	if err != nil {
		t.Fatalf("Create C: %v", err)
	}

	// Advance past the short ttls but not the long one.
	clk.Advance(10 * time.Minute)

	n, err := s.DeleteExpired(ctx)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n != 2 {
		t.Fatalf("DeleteExpired count = %d, want 2", n)
	}

	if _, err := s.Get(ctx, shortA.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("Get shortA after GC: err=%v, want ErrNotFound", err)
	}
	if _, err := s.Get(ctx, shortB.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("Get shortB after GC: err=%v, want ErrNotFound", err)
	}
	got, err := s.Get(ctx, long.ID)
	if err != nil {
		t.Fatalf("Get long after GC: %v", err)
	}
	if got.Username != "carol" {
		t.Fatalf("long.Username = %q", got.Username)
	}
}

func TestGCLoopExitsOnCtxCancel(t *testing.T) {
	s := newStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.GCLoop(ctx, time.Hour) // huge interval; we only care about ctx cancel
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// good
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("GCLoop did not return within 100ms of ctx cancel")
	}
}
