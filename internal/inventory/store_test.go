package inventory

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"metalkit/internal/sqlitedb"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	// modernc.org/sqlite supports WAL only on real files. Use a per-test temp
	// file so each case starts clean and the WAL path is exercised.
	path := filepath.Join(t.TempDir(), "inv.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
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

func sampleReport(uuid string) *Report {
	return &Report{
		SchemaVersion: SchemaVersion,
		AgentVersion:  "test",
		CollectedAt:   time.Now().UTC(),
		Machine: Machine{
			SMBIOSUUID:   uuid,
			Manufacturer: "Dell Inc.",
			ProductName:  "PowerEdge R740",
			Serial:       "ABC123",
		},
		NICs: []NIC{
			{Name: "eno1", MAC: "AA:BB:CC:DD:EE:01"},
			{Name: "eno2", MAC: "aa:bb:cc:dd:ee:02"},
		},
		BMC: &BMC{
			MAC: "AA:BB:CC:DD:EE:FF",
			IP:  "10.0.0.5",
		},
		System: System{KernelRelease: "6.6.0-test"},
	}
}

func TestOpenCreatesSchema(t *testing.T) {
	s := newTestStore(t)
	machines, err := s.ListMachines(context.Background())
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(machines) != 0 {
		t.Fatalf("expected empty, got %d", len(machines))
	}
}

func TestUpsertReport(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const u = "11111111-2222-3333-4444-555555555555"

	uuid, reportID, err := s.UpsertReport(ctx, sampleReport(u))
	if err != nil {
		t.Fatalf("UpsertReport: %v", err)
	}
	if uuid != u {
		t.Fatalf("uuid: got %q want %q", uuid, u)
	}
	if reportID <= 0 {
		t.Fatalf("reportID: got %d", reportID)
	}

	ms, err := s.ListMachines(ctx)
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(ms) != 1 || ms[0].UUID != u {
		t.Fatalf("ListMachines: %+v", ms)
	}
	if ms[0].Manufacturer != "Dell Inc." || ms[0].ProductName != "PowerEdge R740" {
		t.Fatalf("identity fields lost: %+v", ms[0])
	}
	if ms[0].Status != "online" {
		t.Fatalf("status: %q", ms[0].Status)
	}
	if ms[0].LatestReport != reportID {
		t.Fatalf("latest_report: got %d want %d", ms[0].LatestReport, reportID)
	}

	// MAC lookup: BMC and NIC paths each resolve and tag the right role.
	for _, tc := range []struct {
		mac, role string
	}{
		{"aa:bb:cc:dd:ee:01", "nic"},
		{"AA:BB:CC:DD:EE:02", "nic"},
		{"aa:bb:cc:dd:ee:ff", "bmc"},
	} {
		m, err := s.LookupByMAC(ctx, tc.mac)
		if err != nil {
			t.Fatalf("LookupByMAC %s: %v", tc.mac, err)
		}
		if m.UUID != u || m.Role != tc.role {
			t.Fatalf("LookupByMAC %s: got %+v, want uuid=%s role=%s", tc.mac, m, u, tc.role)
		}
	}

	if _, err := s.LookupByMAC(ctx, "00:00:00:00:00:00"); err != ErrNotFound {
		t.Fatalf("unknown mac: got %v want ErrNotFound", err)
	}
}

func TestUpsertReportPreservesFirstSeen(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const u = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	if _, _, err := s.UpsertReport(ctx, sampleReport(u)); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	ms, _ := s.ListMachines(ctx)
	firstSeen := ms[0].FirstSeen
	firstLastSeen := ms[0].LastSeen

	// Sleep a real second so unix-second timestamps differ.
	time.Sleep(1100 * time.Millisecond)

	if _, _, err := s.UpsertReport(ctx, sampleReport(u)); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	ms, _ = s.ListMachines(ctx)
	if !ms[0].FirstSeen.Equal(firstSeen) {
		t.Fatalf("FirstSeen changed: was %v now %v", firstSeen, ms[0].FirstSeen)
	}
	if !ms[0].LastSeen.After(firstLastSeen) {
		t.Fatalf("LastSeen did not advance: was %v now %v", firstLastSeen, ms[0].LastSeen)
	}
}

func TestHeartbeatNotFound(t *testing.T) {
	s := newTestStore(t)
	err := s.Heartbeat(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != ErrNotFound {
		t.Fatalf("Heartbeat unknown: got %v want ErrNotFound", err)
	}
}

func TestHeartbeatExisting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const u = "12345678-1234-1234-1234-123456789abc"
	if _, _, err := s.UpsertReport(ctx, sampleReport(u)); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Force the machine offline so we can see the heartbeat flip it back.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE machines SET status = 'offline', last_seen = ? WHERE uuid = ?`,
		time.Now().Add(-1*time.Hour).Unix(), u); err != nil {
		t.Fatalf("setup: %v", err)
	}

	time.Sleep(1100 * time.Millisecond)
	if err := s.Heartbeat(ctx, u); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	ms, _ := s.ListMachines(ctx)
	if ms[0].Status != "online" {
		t.Fatalf("status: %q", ms[0].Status)
	}
	if time.Since(ms[0].LastSeen) > 5*time.Second {
		t.Fatalf("LastSeen not refreshed: %v", ms[0].LastSeen)
	}

	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM heartbeats WHERE uuid = ?`, u).Scan(&n); err != nil {
		t.Fatalf("count heartbeats: %v", err)
	}
	if n != 1 {
		t.Fatalf("heartbeats: got %d want 1", n)
	}
}

func TestReportsHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const u = "99999999-8888-7777-6666-555555555555"

	var ids []int64
	for i := 0; i < 3; i++ {
		_, id, err := s.UpsertReport(ctx, sampleReport(u))
		if err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
		ids = append(ids, id)
		time.Sleep(1100 * time.Millisecond)
	}

	metas, err := s.ListReports(ctx, u)
	if err != nil {
		t.Fatalf("ListReports: %v", err)
	}
	if len(metas) != 3 {
		t.Fatalf("ListReports: got %d want 3", len(metas))
	}
	// Newest first.
	if metas[0].ID != ids[2] || metas[2].ID != ids[0] {
		t.Fatalf("ListReports order: %+v", metas)
	}

	rep, err := s.GetReport(ctx, u, ids[1])
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if rep.Machine.SMBIOSUUID != u {
		t.Fatalf("GetReport returned wrong machine: %q", rep.Machine.SMBIOSUUID)
	}

	if _, err := s.GetReport(ctx, u, 99999); err != ErrNotFound {
		t.Fatalf("GetReport bad id: got %v want ErrNotFound", err)
	}
}

func TestLatestReportNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.LatestReport(context.Background(),
		"00000000-0000-0000-0000-000000000000"); err != ErrNotFound {
		t.Fatalf("LatestReport unknown: got %v want ErrNotFound", err)
	}
}

func TestOfflineMarker(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const u = "abcdef01-0000-0000-0000-000000000000"

	if _, _, err := s.UpsertReport(ctx, sampleReport(u)); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Shove last_seen far enough into the past that the cutoff fires.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE machines SET last_seen = ? WHERE uuid = ?`,
		time.Now().Add(-200*time.Second).Unix(), u); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := s.markOfflineOnce(ctx)
	if err != nil {
		t.Fatalf("markOfflineOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("transitioned: got %d want 1", n)
	}

	var status string
	if err := s.db.QueryRowContext(ctx,
		`SELECT status FROM machines WHERE uuid = ?`, u).Scan(&status); err != nil {
		t.Fatalf("status query: %v", err)
	}
	if status != "offline" {
		t.Fatalf("status: %q", status)
	}

	// Idempotent: a second pass should not re-mark anyone.
	n2, err := s.markOfflineOnce(ctx)
	if err != nil {
		t.Fatalf("markOfflineOnce 2: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second pass: got %d want 0", n2)
	}
}

func TestUpsertReportMACChurn(t *testing.T) {
	// A second upsert with a different MAC set replaces, not unions, the index —
	// otherwise a swapped NIC card would haunt LookupByMAC forever.
	s := newTestStore(t)
	ctx := context.Background()
	const u = "fedcba98-7654-3210-fedc-ba9876543210"

	r1 := sampleReport(u)
	if _, _, err := s.UpsertReport(ctx, r1); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	r2 := sampleReport(u)
	r2.NICs = []NIC{{Name: "eno1", MAC: "11:22:33:44:55:66"}}
	r2.BMC = nil
	if _, _, err := s.UpsertReport(ctx, r2); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	if _, err := s.LookupByMAC(ctx, "aa:bb:cc:dd:ee:01"); err != ErrNotFound {
		t.Fatalf("old NIC mac still indexed: got %v", err)
	}
	if _, err := s.LookupByMAC(ctx, "aa:bb:cc:dd:ee:ff"); err != ErrNotFound {
		t.Fatalf("old BMC mac still indexed: got %v", err)
	}
	m, err := s.LookupByMAC(ctx, "11:22:33:44:55:66")
	if err != nil || m.UUID != u {
		t.Fatalf("new NIC mac: got %+v err=%v", m, err)
	}
}

// Sanity check: the driver and pragmas registered as we expect.
func TestPragmas(t *testing.T) {
	s := newTestStore(t)
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode: got %q want wal", mode)
	}
	var fk int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys: got %d want 1", fk)
	}
}
