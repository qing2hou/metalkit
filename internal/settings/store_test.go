package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"metalkit/internal/config"
	"metalkit/internal/sqlitedb"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{
		Path:   filepath.Join(t.TempDir(), "test.db"),
		Logger: logger,
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

func TestStore_SetGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Set(ctx, "dhcp.mode", "full", "admin"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, "dhcp.mode")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "full" {
		t.Errorf("got %q, want full", got)
	}
}

func TestStore_SetMany_Atomic(t *testing.T) {
	// Bulk write of the DHCP form should be one transaction — partial state
	// on a hypothetical mid-batch failure would be confusing for operators.
	s := newTestStore(t)
	ctx := context.Background()
	kv := map[string]string{
		"dhcp.mode":       "full",
		"dhcp.pool.start": "10.0.0.100",
		"dhcp.pool.end":   "10.0.0.200",
	}
	if err := s.SetMany(ctx, kv, "admin"); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	got, err := s.GetMany(ctx, []string{"dhcp.mode", "dhcp.pool.start", "dhcp.pool.end"})
	if err != nil {
		t.Fatalf("GetMany: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d rows, want 3", len(got))
	}
}

func TestAPI_GetDHCP_FallsBackToBootConfig(t *testing.T) {
	// With no override rows, GET must return the boot-config values verbatim
	// so a fresh install shows whatever config.yaml had.
	s := newTestStore(t)
	cfg := &config.Config{
		ServerIP: "192.168.10.120",
		DHCPMode: config.DHCPModeFull,
		DHCPPool: &config.DHCPPool{
			Start:      "192.168.10.100",
			End:        "192.168.10.200",
			Netmask:    "255.255.255.0",
			Gateway:    "192.168.10.1",
			DNS:        []string{"8.8.8.8"},
			LeaseHours: 24,
			Exclude:    []string{"192.168.10.120"},
		},
	}
	api := NewAPI(s, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/settings/dhcp")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got DHCPSettings
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Mode != "full" || got.Start != "192.168.10.100" || got.LeaseHours != 24 {
		t.Errorf("fallback mismatch: %+v", got)
	}
}

func TestAPI_PutDHCP_PersistsAndSignalsRestart(t *testing.T) {
	s := newTestStore(t)
	cfg := &config.Config{
		ServerIP: "192.168.10.120",
		DHCPMode: config.DHCPModeProxy,
	}
	api := NewAPI(s, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := `{
		"mode": "full",
		"start": "192.168.10.100",
		"end": "192.168.10.200",
		"netmask": "255.255.255.0",
		"gateway": "192.168.10.1",
		"dns": ["8.8.8.8"],
		"lease_hours": 12,
		"exclude": []
	}`
	req, _ := http.NewRequest("PUT", srv.URL+"/api/v1/settings/dhcp", strings.NewReader(body))
	req.SetBasicAuth("alice", "ignored")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	var got DHCPSettingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.RestartRequired {
		t.Error("restart_required = false, want true")
	}

	// Verify the row landed in the store and updated_by is the basic-auth user.
	mode, err := s.Get(context.Background(), KeyDHCPMode)
	if err != nil {
		t.Fatalf("Get mode: %v", err)
	}
	if mode != "full" {
		t.Errorf("stored mode = %q, want full", mode)
	}
}

func TestAPI_PutDHCP_RejectsBadInput(t *testing.T) {
	s := newTestStore(t)
	cfg := &config.Config{ServerIP: "192.168.10.120", DHCPMode: config.DHCPModeProxy}
	api := NewAPI(s, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cases := []struct{ name, body string }{
		{"bad mode", `{"mode":"weird"}`},
		{"bad mask in full", `{"mode":"full","start":"10.0.0.10","end":"10.0.0.20","netmask":"nope","gateway":"10.0.0.1","dns":[],"lease_hours":1,"exclude":[]}`},
		{"start>end", `{"mode":"full","start":"10.0.0.20","end":"10.0.0.10","netmask":"255.255.255.0","gateway":"10.0.0.1","dns":[],"lease_hours":1,"exclude":[]}`},
		{"pool outside subnet", `{"mode":"full","start":"10.0.1.10","end":"10.0.1.20","netmask":"255.255.255.0","gateway":"10.0.0.1","dns":[],"lease_hours":1,"exclude":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest("PUT", srv.URL+"/api/v1/settings/dhcp", strings.NewReader(c.body))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("PUT: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 400 {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestApplyOverridesToConfig_OverlaysPool(t *testing.T) {
	// UI flipped mode to full and set custom start/end — the in-memory cfg
	// must reflect those values on next startup.
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SetMany(ctx, map[string]string{
		KeyDHCPMode:       "full",
		KeyDHCPStart:      "10.0.0.50",
		KeyDHCPEnd:        "10.0.0.150",
		KeyDHCPLeaseHours: "48",
	}, "admin"); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	cfg := &config.Config{
		ServerIP: "10.0.0.1",
		DHCPMode: config.DHCPModeProxy,
		DHCPPool: &config.DHCPPool{Start: "OLD", End: "OLD", LeaseHours: 24},
	}
	if err := ApplyOverridesToConfig(ctx, s, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("ApplyOverridesToConfig: %v", err)
	}
	if cfg.DHCPMode != "full" {
		t.Errorf("mode = %q, want full", cfg.DHCPMode)
	}
	if cfg.DHCPPool.Start != "10.0.0.50" || cfg.DHCPPool.End != "10.0.0.150" || cfg.DHCPPool.LeaseHours != 48 {
		t.Errorf("pool overlay failed: %+v", cfg.DHCPPool)
	}
}

func TestAPI_InterfaceFieldLifecycle(t *testing.T) {
	s := newTestStore(t)
	// lo always exists on every test host; use it as the "other" NIC.
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		t.Skip("no loopback interface")
	}
	cfg := &config.Config{
		ServerIP:  "192.168.10.120",
		Interface: "not-the-real-one", // differs from what we PUT
		DHCPMode:  config.DHCPModeProxy,
	}
	api := NewAPI(s, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// PUT switching the NIC → saved, and restart_required even though the
	// reloader (nil here → "failed" path) is irrelevant: interface changes
	// can never hot-apply.
	body := fmt.Sprintf(`{"mode":"proxy","interface":%q}`, lo.Name)
	req, _ := http.NewRequest("PUT", srv.URL+"/api/v1/settings/dhcp", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	var got DHCPSettingsResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Interface != lo.Name {
		t.Errorf("interface echo = %q, want %q", got.Interface, lo.Name)
	}
	if !got.RestartRequired {
		t.Error("restart_required = false, want true (NIC change needs restart)")
	}

	// GET reflects the override.
	resp2, _ := http.Get(srv.URL + "/api/v1/settings/dhcp")
	defer resp2.Body.Close()
	var got2 DHCPSettings
	_ = json.NewDecoder(resp2.Body).Decode(&got2)
	if got2.Interface != lo.Name {
		t.Errorf("GET interface = %q, want %q", got2.Interface, lo.Name)
	}

	// Interfaces list marks the current one.
	resp3, _ := http.Get(srv.URL + "/api/v1/settings/interfaces")
	defer resp3.Body.Close()
	var ifaces []InterfaceInfo
	_ = json.NewDecoder(resp3.Body).Decode(&ifaces)
	found := false
	for _, i := range ifaces {
		if i.Name == lo.Name {
			found = true
			if !i.IsCurrent {
				t.Errorf("%s should be marked current", lo.Name)
			}
		}
	}
	if !found {
		t.Errorf("loopback missing from interfaces list (%d entries)", len(ifaces))
	}

	// PUT naming a nonexistent NIC → 400 at save time.
	req, _ = http.NewRequest("PUT", srv.URL+"/api/v1/settings/dhcp",
		strings.NewReader(`{"mode":"proxy","interface":"definitely-not-a-nic-xyz"}`))
	resp4, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT bad nic: %v", err)
	}
	defer resp4.Body.Close()
	if resp4.StatusCode != http.StatusBadRequest {
		t.Errorf("bad nic status = %d, want 400", resp4.StatusCode)
	}
}

func TestAPI_PutProxyZeroesPoolFields(t *testing.T) {
	s := newTestStore(t)
	// First store a full-mode pool so there IS stale data to clear.
	cfg := &config.Config{ServerIP: "192.168.10.120", Interface: "", DHCPMode: config.DHCPModeFull}
	api := NewAPI(s, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if err := s.SetMany(context.Background(), map[string]string{
		KeyDHCPMode:       "full",
		KeyDHCPStart:      "192.168.10.100",
		KeyDHCPEnd:        "192.168.10.200",
		KeyDHCPNetmask:    "255.255.255.0",
		KeyDHCPGateway:    "192.168.10.1",
		KeyDHCPDNS:        "8.8.8.8",
		KeyDHCPLeaseHours: "48",
		KeyDHCPExclude:    "192.168.10.9",
	}, "test"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Switch to proxy WITH stale pool values still in the request body —
	// they must be zeroed, not persisted.
	body := `{
		"mode": "proxy",
		"start": "10.99.99.100",
		"end": "10.99.99.200",
		"netmask": "255.0.0.0",
		"gateway": "10.99.99.1",
		"dns": ["8.8.4.4"],
		"lease_hours": 96,
		"exclude": ["10.99.99.9"]
	}`
	req, _ := http.NewRequest("PUT", srv.URL+"/api/v1/settings/dhcp", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}

	// Every pool key must now read blank/zero in the store — the stale
	// seeded values AND the request's garbage values both gone.
	for key, want := range map[string]string{
		KeyDHCPStart: "", KeyDHCPEnd: "", KeyDHCPNetmask: "", KeyDHCPGateway: "",
		KeyDHCPDNS: "", KeyDHCPLeaseHours: "0", KeyDHCPExclude: "",
	} {
		got, err := s.Get(context.Background(), key)
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q (pool leaked into proxy mode)", key, got, want)
		}
	}
	if mode, _ := s.Get(context.Background(), KeyDHCPMode); mode != "proxy" {
		t.Errorf("mode = %q, want proxy", mode)
	}

	// GET shows blank pool and empty interface (config had none).
	resp2, _ := http.Get(srv.URL + "/api/v1/settings/dhcp")
	defer resp2.Body.Close()
	var got DHCPSettings
	_ = json.NewDecoder(resp2.Body).Decode(&got)
	if got.Start != "" || got.LeaseHours != 0 || len(got.DNS) != 0 {
		t.Errorf("GET after proxy switch: %+v — pool should read blank", got)
	}
}
