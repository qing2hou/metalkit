package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := newTestStore(t)
	mux := http.NewServeMux()
	RegisterRoutes(mux, s, logger)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestPostReportHappyPath(t *testing.T) {
	srv, _ := newTestServer(t)
	rep := sampleReport("11111111-2222-3333-4444-555555555555")
	resp, err := http.Post(srv.URL+"/api/v1/report", "application/json",
		bytes.NewReader(mustJSON(t, rep)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var got struct {
		UUID     string `json:"uuid"`
		ReportID int64  `json:"report_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UUID != rep.Machine.SMBIOSUUID || got.ReportID == 0 {
		t.Fatalf("response: %+v", got)
	}

	// And it shows up in /machines.
	resp2, err := http.Get(srv.URL + "/api/v1/machines")
	if err != nil {
		t.Fatalf("GET machines: %v", err)
	}
	defer resp2.Body.Close()
	var machines []MachineSummary
	if err := json.NewDecoder(resp2.Body).Decode(&machines); err != nil {
		t.Fatalf("decode machines: %v", err)
	}
	if len(machines) != 1 || machines[0].UUID != rep.Machine.SMBIOSUUID {
		t.Fatalf("machines: %+v", machines)
	}
}

func TestPostReportMissingUUID(t *testing.T) {
	srv, _ := newTestServer(t)
	rep := sampleReport("")
	resp, err := http.Post(srv.URL+"/api/v1/report", "application/json",
		bytes.NewReader(mustJSON(t, rep)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestPostReportBadSchema(t *testing.T) {
	srv, _ := newTestServer(t)
	rep := sampleReport("11111111-2222-3333-4444-555555555555")
	rep.SchemaVersion = 99
	resp, err := http.Post(srv.URL+"/api/v1/report", "application/json",
		bytes.NewReader(mustJSON(t, rep)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestPostReportOversized(t *testing.T) {
	srv, _ := newTestServer(t)
	// 6 MiB of valid-looking JSON body. The encoder won't blow past the cap;
	// we deliberately build a raw oversized body so MaxBytesReader trips.
	big := bytes.Repeat([]byte("a"), 6*1024*1024)
	body := append([]byte(`{"padding":"`), big...)
	body = append(body, []byte(`"}`)...)

	resp, err := http.Post(srv.URL+"/api/v1/report", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: %d want 413", resp.StatusCode)
	}
}

func TestPostReportInvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Post(srv.URL+"/api/v1/report", "application/json",
		strings.NewReader("not json"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestHeartbeatRoutes(t *testing.T) {
	srv, store := newTestServer(t)
	const u = "33333333-4444-5555-6666-777777777777"
	if _, _, err := store.UpsertReport(context.Background(), sampleReport(u)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Existing -> 204.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/heartbeat/"+u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("existing heartbeat: %d", resp.StatusCode)
	}

	// Unknown uuid (well-formed) -> 404.
	req, _ = http.NewRequest(http.MethodPost,
		srv.URL+"/api/v1/heartbeat/00000000-0000-0000-0000-000000000000", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing heartbeat: %d", resp.StatusCode)
	}

	// Malformed uuid -> 400.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/v1/heartbeat/not-a-uuid", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad uuid heartbeat: %d", resp.StatusCode)
	}
}

func TestListMachines(t *testing.T) {
	srv, store := newTestServer(t)

	resp, err := http.Get(srv.URL + "/api/v1/machines")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var first []MachineSummary
	_ = json.NewDecoder(resp.Body).Decode(&first)
	resp.Body.Close()
	if len(first) != 0 {
		t.Fatalf("initial: %+v", first)
	}

	if _, _, err := store.UpsertReport(context.Background(),
		sampleReport("44444444-4444-4444-4444-444444444444")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp, err = http.Get(srv.URL + "/api/v1/machines")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var second []MachineSummary
	if err := json.NewDecoder(resp.Body).Decode(&second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if len(second) != 1 {
		t.Fatalf("after seed: %+v", second)
	}
}

func TestGetMachine(t *testing.T) {
	srv, store := newTestServer(t)
	const u = "55555555-6666-7777-8888-999999999999"

	// Unknown -> 404.
	resp, err := http.Get(srv.URL + "/api/v1/machines/" + u)
	if err != nil {
		t.Fatalf("GET unknown: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown: %d", resp.StatusCode)
	}

	if _, _, err := store.UpsertReport(context.Background(), sampleReport(u)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Known -> 200 and round-trips the SMBIOSUUID.
	resp, err = http.Get(srv.URL + "/api/v1/machines/" + u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var rep Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Machine.SMBIOSUUID != u {
		t.Fatalf("uuid mismatch: %q", rep.Machine.SMBIOSUUID)
	}

	// Bad uuid -> 400.
	resp, err = http.Get(srv.URL + "/api/v1/machines/not-a-uuid")
	if err != nil {
		t.Fatalf("GET bad: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad uuid: %d", resp.StatusCode)
	}
}

func TestListReportsRoute(t *testing.T) {
	srv, store := newTestServer(t)
	ctx := context.Background()
	const u = "66666666-7777-8888-9999-aaaaaaaaaaaa"

	// Unknown machine -> 404.
	resp, err := http.Get(srv.URL + "/api/v1/machines/" + u + "/reports")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown machine: %d", resp.StatusCode)
	}

	var ids []int64
	for i := 0; i < 2; i++ {
		_, id, err := store.UpsertReport(ctx, sampleReport(u))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids = append(ids, id)
		time.Sleep(1100 * time.Millisecond)
	}

	resp, err = http.Get(srv.URL + "/api/v1/machines/" + u + "/reports")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var metas []ReportMeta
	if err := json.NewDecoder(resp.Body).Decode(&metas); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("metas: %+v", metas)
	}

	// Fetch a specific report by id.
	resp, err = http.Get(fmt.Sprintf("%s/api/v1/machines/%s/reports/%d", srv.URL, u, ids[0]))
	if err != nil {
		t.Fatalf("GET report: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var rep Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Machine.SMBIOSUUID != u {
		t.Fatalf("uuid: %q", rep.Machine.SMBIOSUUID)
	}
}

func TestLookupRoute(t *testing.T) {
	srv, store := newTestServer(t)
	const u = "77777777-8888-9999-aaaa-bbbbbbbbbbbb"
	if _, _, err := store.UpsertReport(context.Background(), sampleReport(u)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// NIC MAC, mixed case in the query.
	resp, err := http.Get(srv.URL + "/api/v1/lookup?mac=AA:BB:CC:DD:EE:01")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var m MacMatch
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if m.UUID != u || m.Role != "nic" {
		t.Fatalf("nic lookup: %+v", m)
	}

	// BMC MAC.
	resp, err = http.Get(srv.URL + "/api/v1/lookup?mac=aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if m.UUID != u || m.Role != "bmc" {
		t.Fatalf("bmc lookup: %+v", m)
	}

	// Unknown -> 404.
	resp, err = http.Get(srv.URL + "/api/v1/lookup?mac=99:99:99:99:99:99")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown mac: %d", resp.StatusCode)
	}

	// Malformed -> 400.
	resp, err = http.Get(srv.URL + "/api/v1/lookup?mac=not-a-mac")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad mac: %d", resp.StatusCode)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	// http.ServeMux's method-aware patterns reject the wrong verb with 405.
	srv, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/report", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /report: %d", resp.StatusCode)
	}
}
