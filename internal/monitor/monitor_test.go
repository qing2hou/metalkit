package monitor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metalkit/internal/sqlitedb"
)

// memFS serves a fixed file map — enough to exercise the /proc parsers
// against crafted contents.
type memFS map[string]string

func (m memFS) ReadFile(name string) ([]byte, error) {
	if v, ok := m[name]; ok {
		return []byte(v), nil
	}
	return nil, &osPathError{name: name}
}

type osPathError struct{ name string }

func (e *osPathError) Error() string { return "not found: " + e.name }

func testFS() memFS {
	return memFS{
		"/proc/stat": "cpu  100 0 50 800 20 0 0 10 0 0\n" +
			"cpu0 50 0 25 400 10 0 0 5 0 0\n" +
			"cpu1 50 0 25 400 10 0 0 5 0 0\n" +
			"intr 12345\n",
		"/proc/loadavg":                  "0.52 0.58 0.59 1/500 12345\n",
		"/proc/meminfo":                  "MemTotal:       16000000 kB\nMemAvailable:    8000000 kB\nSwapTotal:       2000000 kB\nSwapFree:        1900000 kB\n",
		"/sys/devices/system/cpu/online": "0-3\n",
		"/proc/diskstats": "   8       0 sda 12345 100 500000 1000 20000 150 800000 2000 1 3000 4000 0 0 0 0\n" +
			" 259       0 nvme0n1 500 10 20000 100 900 20 30000 200 0 400 500 0 0 0 0\n",
		"/proc/net/dev": "Inter-|   Receive                                                |  Transmit\n" +
			" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
			"  lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0\n" +
			"  eth0: 123456 1000 0 0 0 0 0 0 654321 900 0 0 0 0 0 0\n",
		"/proc/uptime": "12345.67 8901.23\n",
	}
}

func TestCollectParsers(t *testing.T) {
	m := Collect(testFS())

	if m.CPUs != 4 {
		t.Errorf("CPUs = %d, want 4 (from 0-3 cpu list)", m.CPUs)
	}
	// total = 100+0+50+800+20+0+0+10 = 980; idle = 800+20 = 820
	if m.CPUTotalJiffies != 980 {
		t.Errorf("CPUTotalJiffies = %d, want 980", m.CPUTotalJiffies)
	}
	if m.CPUIdleJiffies != 820 {
		t.Errorf("CPUIdleJiffies = %d, want 820", m.CPUIdleJiffies)
	}
	if m.Loadavg1 != 0.52 || m.Loadavg5 != 0.58 || m.Loadavg15 != 0.59 {
		t.Errorf("loadavg = %v/%v/%v, want 0.52/0.58/0.59", m.Loadavg1, m.Loadavg5, m.Loadavg15)
	}
	if m.MemTotalKB != 16000000 || m.MemAvailableKB != 8000000 {
		t.Errorf("mem = %d/%d, want 16000000/8000000", m.MemTotalKB, m.MemAvailableKB)
	}
	if m.SwapTotalKB != 2000000 || m.SwapFreeKB != 1900000 {
		t.Errorf("swap = %d/%d, want 2000000/1900000", m.SwapTotalKB, m.SwapFreeKB)
	}
	if m.BootUptimeSeconds != 12345.67 {
		t.Errorf("uptime = %v, want 12345.67", m.BootUptimeSeconds)
	}
	if len(m.Disks) != 2 {
		t.Fatalf("disks = %d, want 2", len(m.Disks))
	}
	if m.Disks[0].Device != "sda" || m.Disks[0].ReadSectors != 500000 {
		t.Errorf("disk[0] = %+v, want sda/500000 read sectors", m.Disks[0])
	}
	if len(m.Interfaces) != 1 { // lo excluded
		t.Fatalf("interfaces = %d, want 1 (lo excluded)", len(m.Interfaces))
	}
	if m.Interfaces[0].Name != "eth0" || m.Interfaces[0].Bytes.Rx != 123456 || m.Interfaces[0].Bytes.Tx != 654321 {
		t.Errorf("iface[0] = %+v, want eth0 rx=123456 tx=654321", m.Interfaces[0])
	}
}

// CPU count falls back to /proc/stat cpuN lines when sysfs is missing.
func TestCollectCPUCountFallback(t *testing.T) {
	fs := testFS()
	delete(fs, "/sys/devices/system/cpu/online")
	if got := Collect(fs).CPUs; got != 2 {
		t.Errorf("CPUs fallback = %d, want 2 (cpu0+cpu1 lines)", got)
	}
}

// A missing file contributes zeros — Collect never errors.
func TestCollectMissingFiles(t *testing.T) {
	m := Collect(memFS{})
	if m.CPUs != 0 || m.MemTotalKB != 0 || len(m.Disks) != 0 {
		t.Errorf("empty fs should yield zero metrics, got %+v", m)
	}
}

func TestCountCPUList(t *testing.T) {
	cases := map[string]uint64{
		"0":           1,
		"0-3":         4,
		"0-3,8":       5,
		"0-3,8,10-11": 7,
		"":            0,
	}
	for in, want := range cases {
		if got := countCPUList(in); got != want {
			t.Errorf("countCPUList(%q) = %d, want %d", in, got, want)
		}
	}
}

// ---- store + API round-trip ----

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
		t.Fatalf("new store: %v", err)
	}
	return s
}

func TestStoreAppendAndHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		p := Payload{
			MachineUUID:   "AAAA-BBBB",
			SchemaVersion: SchemaVersion,
			MonitoredAt:   time.Unix(int64(1700000000+i*60), 0).UTC(),
			Metrics:       Metrics{CPUs: 4, MemTotalKB: 1000},
		}
		if err := s.Append(ctx, strings.ToLower("AAAA-BBBB"), p); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	hist, err := s.History(ctx, "aaaa-bbbb", 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("history len = %d, want 3", len(hist))
	}
	for i, p := range hist {
		if want := int64(1700000000 + i*60); p.MonitoredAt.Unix() != want {
			t.Errorf("hist[%d].MonitoredAt = %d, want %d", i, p.MonitoredAt.Unix(), want)
		}
	}
}

func TestStoreAppendUUIDMismatch(t *testing.T) {
	s := newTestStore(t)
	err := s.Append(context.Background(), "uuid-a", Payload{MachineUUID: "uuid-b"})
	if err == nil {
		t.Fatal("append with mismatched uuid should fail")
	}
}

func TestStorePrune(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	old := Payload{MachineUUID: "u", MonitoredAt: time.Unix(1000000, 0).UTC()}
	fresh := Payload{MachineUUID: "u", MonitoredAt: time.Now().UTC()}
	if err := s.Append(ctx, "u", old); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, "u", fresh); err != nil {
		t.Fatal(err)
	}
	n, err := s.Prune(ctx, time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	hist, _ := s.History(ctx, "u", 10)
	if len(hist) != 1 {
		t.Fatalf("post-prune history len = %d, want 1", len(hist))
	}
}

func TestStoreLatestWithin(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fresh := Payload{MachineUUID: "ok", MonitoredAt: time.Now().UTC()}
	stale := Payload{MachineUUID: "gone", MonitoredAt: time.Now().Add(-time.Hour).UTC()}
	_ = s.Append(ctx, "ok", fresh)
	_ = s.Append(ctx, "gone", stale)
	m, err := s.Latest(ctx, 3*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["ok"]; !ok {
		t.Error("fresh machine missing from Latest")
	}
	if _, ok := m["gone"]; ok {
		t.Error("stale machine present in Latest")
	}
}

func TestAPIPushAndQuery(t *testing.T) {
	s := newTestStore(t)
	api := NewAPI(s, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Push without auth (agent path is open by design at this layer).
	p := CollectPayload(testFS(), "test-uuid")
	body, _ := json.Marshal(p)
	resp, err := http.Post(srv.URL+"/api/v1/agent/metrics", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("push status = %d, want 204", resp.StatusCode)
	}

	// Bad payload: missing machine_uuid.
	bad, _ := json.Marshal(Payload{SchemaVersion: SchemaVersion})
	resp, err = http.Post(srv.URL+"/api/v1/agent/metrics", "application/json", strings.NewReader(string(bad)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad push status = %d, want 400", resp.StatusCode)
	}

	// Wrong schema_version.
	wrong, _ := json.Marshal(Payload{MachineUUID: "x", SchemaVersion: 999})
	resp, err = http.Post(srv.URL+"/api/v1/agent/metrics", "application/json", strings.NewReader(string(wrong)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong-schema push status = %d, want 400", resp.StatusCode)
	}

	// Latest listing contains the pushed machine.
	resp, err = http.Get(srv.URL + "/api/v1/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var samples []Sample
	if err := json.NewDecoder(resp.Body).Decode(&samples); err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].MachineUUID != "test-uuid" || !samples[0].AgentOnline {
		t.Fatalf("latest = %+v, want one online test-uuid", samples)
	}

	// History endpoint.
	resp, err = http.Get(srv.URL + "/api/v1/metrics/test-uuid?limit=5")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var hist []Payload
	if err := json.NewDecoder(resp.Body).Decode(&hist); err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("history len = %d, want 1", len(hist))
	}
}
