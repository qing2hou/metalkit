package leases

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"metalkit/internal/sqlitedb"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{Path: path, Logger: logger})
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

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a
}

func defaultPool(t *testing.T) AllocateInput {
	t.Helper()
	return AllocateInput{
		Start:    mustAddr(t, "192.168.10.100"),
		End:      mustAddr(t, "192.168.10.105"),
		Exclude:  map[string]struct{}{"192.168.10.101": {}},
		LeaseDur: time.Hour,
	}
}

func TestAllocate_LowestFree(t *testing.T) {
	// New MAC should get the lowest non-excluded IP (.100) on first allocate;
	// a second MAC should skip the excluded .101 and land on .102.
	s := newTestStore(t)
	ctx := context.Background()
	in := defaultPool(t)
	in.MAC = "aa:bb:cc:dd:ee:01"
	l, err := s.Allocate(ctx, in)
	if err != nil {
		t.Fatalf("alloc 1: %v", err)
	}
	if l.IP != "192.168.10.100" {
		t.Errorf("first alloc IP = %q, want 192.168.10.100", l.IP)
	}
	if l.State != "offered" {
		t.Errorf("state = %q, want offered", l.State)
	}

	in.MAC = "aa:bb:cc:dd:ee:02"
	l2, err := s.Allocate(ctx, in)
	if err != nil {
		t.Fatalf("alloc 2: %v", err)
	}
	if l2.IP != "192.168.10.102" {
		t.Errorf("second alloc IP = %q, want 192.168.10.102 (skipped .101 exclude)", l2.IP)
	}
}

func TestAllocate_StickyByMAC(t *testing.T) {
	// Re-allocating for the same MAC returns the same IP.
	s := newTestStore(t)
	ctx := context.Background()
	in := defaultPool(t)
	in.MAC = "aa:bb:cc:dd:ee:01"

	l1, err := s.Allocate(ctx, in)
	if err != nil {
		t.Fatalf("alloc 1: %v", err)
	}
	l2, err := s.Allocate(ctx, in)
	if err != nil {
		t.Fatalf("alloc 2: %v", err)
	}
	if l1.IP != l2.IP {
		t.Errorf("sticky failed: l1=%s l2=%s", l1.IP, l2.IP)
	}
}

func TestAllocate_CaseInsensitiveMAC(t *testing.T) {
	// Same MAC in upper/lower case should hit the same row, not duplicate it.
	s := newTestStore(t)
	ctx := context.Background()
	in := defaultPool(t)

	in.MAC = "AA:BB:CC:DD:EE:01"
	l1, err := s.Allocate(ctx, in)
	if err != nil {
		t.Fatalf("alloc 1: %v", err)
	}
	in.MAC = "aa:bb:cc:dd:ee:01"
	l2, err := s.Allocate(ctx, in)
	if err != nil {
		t.Fatalf("alloc 2: %v", err)
	}
	if l1.IP != l2.IP {
		t.Errorf("case folding failed: l1=%s l2=%s", l1.IP, l2.IP)
	}
}

func TestAllocate_PoolExhausted(t *testing.T) {
	// Pool of 2 usable IPs (.100, .102 — .101 excluded) should let us hand
	// out exactly 2 leases then return ErrPoolEmpty for a third MAC.
	s := newTestStore(t)
	ctx := context.Background()
	in := AllocateInput{
		Start:    mustAddr(t, "192.168.10.100"),
		End:      mustAddr(t, "192.168.10.102"),
		Exclude:  map[string]struct{}{"192.168.10.101": {}},
		LeaseDur: time.Hour,
	}
	for i, mac := range []string{"aa:00:00:00:00:01", "aa:00:00:00:00:02"} {
		in.MAC = mac
		if _, err := s.Allocate(ctx, in); err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
	}
	in.MAC = "aa:00:00:00:00:03"
	_, err := s.Allocate(ctx, in)
	if !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("want ErrPoolEmpty, got %v", err)
	}
}

func TestConfirm_HappyPath(t *testing.T) {
	// Allocate (offered) → Confirm (active) — state and expires_at both update.
	s := newTestStore(t)
	ctx := context.Background()
	in := defaultPool(t)
	in.MAC = "aa:bb:cc:dd:ee:01"
	offered, err := s.Allocate(ctx, in)
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	confirmed, err := s.Confirm(ctx, in.MAC, offered.IP, time.Hour)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if confirmed.State != "active" {
		t.Errorf("state = %q, want active", confirmed.State)
	}
	if !confirmed.ExpiresAt.After(offered.ExpiresAt) {
		t.Errorf("confirm did not extend expires_at: offered=%v confirmed=%v",
			offered.ExpiresAt, confirmed.ExpiresAt)
	}
}

func TestConfirm_RequestedIPMismatch(t *testing.T) {
	// REQUEST for an IP that doesn't match our OFFER must be rejected so the
	// DHCP layer can NAK and force the client back to DISCOVER.
	s := newTestStore(t)
	ctx := context.Background()
	in := defaultPool(t)
	in.MAC = "aa:bb:cc:dd:ee:01"
	if _, err := s.Allocate(ctx, in); err != nil {
		t.Fatalf("alloc: %v", err)
	}
	_, err := s.Confirm(ctx, in.MAC, "192.168.10.99", time.Hour)
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
}

func TestRelease_FreesIP(t *testing.T) {
	// After Release, the IP is reusable for a different MAC.
	s := newTestStore(t)
	ctx := context.Background()
	in := defaultPool(t)

	in.MAC = "aa:bb:cc:dd:ee:01"
	first, err := s.Allocate(ctx, in)
	if err != nil {
		t.Fatalf("alloc 1: %v", err)
	}
	if err := s.Release(ctx, in.MAC); err != nil {
		t.Fatalf("release: %v", err)
	}

	in.MAC = "aa:bb:cc:dd:ee:02"
	second, err := s.Allocate(ctx, in)
	if err != nil {
		t.Fatalf("alloc 2: %v", err)
	}
	// Released IP becomes the lowest free again, so MAC #02 should land on it.
	if second.IP != first.IP {
		t.Errorf("released IP not reused: first=%s second=%s", first.IP, second.IP)
	}
}

func TestGC_DropsExpired(t *testing.T) {
	// Fast-forward the store's clock so allocated rows are well past expiry,
	// then GC with a tiny keepFor should clean them out.
	s := newTestStore(t)
	ctx := context.Background()
	in := defaultPool(t)
	in.MAC = "aa:bb:cc:dd:ee:01"
	if _, err := s.Allocate(ctx, in); err != nil {
		t.Fatalf("alloc: %v", err)
	}
	// Move "now" 10 minutes into the future so the 1-minute offer expires.
	s.now = func() time.Time { return time.Now().Add(10 * time.Minute).UTC() }
	dropped, err := s.GC(ctx, 0)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if dropped < 1 {
		t.Errorf("expected at least 1 row dropped, got %d", dropped)
	}
}

func TestList_Order(t *testing.T) {
	// List returns rows ordered by IP ascending — small smoke test that the
	// query doesn't crash and orders consistently.
	s := newTestStore(t)
	ctx := context.Background()
	in := defaultPool(t)
	for i, mac := range []string{"aa:00:00:00:00:02", "aa:00:00:00:00:01"} {
		in.MAC = mac
		if _, err := s.Allocate(ctx, in); err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
	}
	rows, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2", len(rows))
	}
	if rows[0].IP > rows[1].IP {
		t.Errorf("rows not sorted by IP: %v", rows)
	}
}
