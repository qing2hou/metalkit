package profiles

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestAPI(t *testing.T) (*API, *httptest.Server) {
	t.Helper()
	s := newTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := NewAPI(s, logger)
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return a, ts
}

func postJSON(t *testing.T, ts *httptest.Server, path string, payload any) (*http.Response, []byte) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func putJSON(t *testing.T, ts *httptest.Server, path string, payload any) (*http.Response, []byte) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func apiValidPayload(name string) map[string]any {
	return map[string]any{
		"name":               name,
		"description":        "via api",
		"hostname_template":  "node-{serial}",
		"root_password_hash": validSha512crypt,
		"target_disk":        map[string]any{"mode": "smallest"},
		"network": map[string]any{
			"method": "static", "prefix_len": 24, "gateway": "10.0.0.1",
			"dns": []string{"1.1.1.1"}, "nic_selector": "auto",
		},
	}
}

func TestAPICreateThenGet(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, body := postJSON(t, ts, "/api/v1/profiles", apiValidPayload("api1"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var p Profile
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !profileIDRE.MatchString(p.ID) {
		t.Errorf("id %q not 32 hex", p.ID)
	}

	// GET by id roundtrips.
	resp2, body2 := getJSON(t, ts, "/api/v1/profiles/"+p.ID)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", resp2.StatusCode, body2)
	}
	var got Profile
	_ = json.Unmarshal(body2, &got)
	if got.Name != p.Name || got.Network.Method != "static" {
		t.Errorf("get mismatch: %+v", got)
	}
}

func getJSON(t *testing.T, ts *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestAPICreateBadJSON(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, err := http.Post(ts.URL+"/api/v1/profiles", "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAPICreateUnknownField(t *testing.T) {
	_, ts := newTestAPI(t)
	p := apiValidPayload("nope")
	p["bogus"] = "x"
	resp, _ := postJSON(t, ts, "/api/v1/profiles", p)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown field", resp.StatusCode)
	}
}

func TestAPICreateDuplicateName(t *testing.T) {
	_, ts := newTestAPI(t)
	postJSON(t, ts, "/api/v1/profiles", apiValidPayload("dup-api"))
	resp, _ := postJSON(t, ts, "/api/v1/profiles", apiValidPayload("dup-api"))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestAPIList(t *testing.T) {
	_, ts := newTestAPI(t)
	for _, n := range []string{"l1", "l2"} {
		postJSON(t, ts, "/api/v1/profiles", apiValidPayload(n))
	}
	resp, body := getJSON(t, ts, "/api/v1/profiles")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	var got []Profile
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2", len(got))
	}
}

func TestAPIUpdate(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, body := postJSON(t, ts, "/api/v1/profiles", apiValidPayload("upd1"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", resp.StatusCode, body)
	}
	var p Profile
	_ = json.Unmarshal(body, &p)
	resp2, body2 := putJSON(t, ts, "/api/v1/profiles/"+p.ID,
		map[string]any{"description": "new desc"})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", resp2.StatusCode, body2)
	}
	var updated Profile
	_ = json.Unmarshal(body2, &updated)
	if updated.Description != "new desc" {
		t.Errorf("description = %q, want %q", updated.Description, "new desc")
	}
	if updated.HostnameTemplate != p.HostnameTemplate {
		t.Errorf("hostname_template should be unchanged: was %q now %q", p.HostnameTemplate, updated.HostnameTemplate)
	}
}

func TestAPIUpdateRejectsBadValue(t *testing.T) {
	_, ts := newTestAPI(t)
	_, body := postJSON(t, ts, "/api/v1/profiles", apiValidPayload("upd2"))
	var p Profile
	_ = json.Unmarshal(body, &p)
	resp, _ := putJSON(t, ts, "/api/v1/profiles/"+p.ID,
		map[string]any{"hostname_template": ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAPIUpdateNotFound(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, _ := putJSON(t, ts, "/api/v1/profiles/00000000000000000000000000000000",
		map[string]any{"description": "x"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAPIDelete(t *testing.T) {
	_, ts := newTestAPI(t)
	_, body := postJSON(t, ts, "/api/v1/profiles", apiValidPayload("del1"))
	var p Profile
	_ = json.Unmarshal(body, &p)

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/profiles/"+p.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	resp2, _ := getJSON(t, ts, "/api/v1/profiles/"+p.ID)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("GET after DELETE: status = %d, want 404", resp2.StatusCode)
	}
}

func TestAPIInvalidID(t *testing.T) {
	_, ts := newTestAPI(t)
	cases := []string{
		"/api/v1/profiles/short",
		"/api/v1/profiles/00000000000000000000000000000000zzzz",
		"/api/v1/profiles/notHEX_NOT_HEX_NOT_HEX_NOT_HEX_XX",
	}
	for _, p := range cases {
		resp, _ := getJSON(t, ts, p)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, want 400", p, resp.StatusCode)
		}
	}
}

func TestAPIBasicAuthUserCaptured(t *testing.T) {
	a, _ := newTestAPI(t)
	// Build a request manually so we can set Basic Auth and verify the
	// stored `created_by` field reflects the authenticated user.
	body, _ := json.Marshal(apiValidPayload("auth1"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/profiles", bytes.NewReader(body))
	req.SetBasicAuth("alice", "secret")
	w := httptest.NewRecorder()
	a.create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var p Profile
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.CreatedBy != "alice" {
		t.Errorf("created_by = %q, want %q", p.CreatedBy, "alice")
	}

	// And without auth, it falls back to "anonymous".
	body2, _ := json.Marshal(apiValidPayload("auth2"))
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/profiles", bytes.NewReader(body2))
	w2 := httptest.NewRecorder()
	a.create(w2, req2)
	var p2 Profile
	_ = json.Unmarshal(w2.Body.Bytes(), &p2)
	if p2.CreatedBy != "anonymous" {
		t.Errorf("created_by = %q, want %q", p2.CreatedBy, "anonymous")
	}
}

// Confirm Get returns 404 for a well-formed id that doesn't exist.
func TestAPIGetMissing(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, _ := getJSON(t, ts, "/api/v1/profiles/ffffffffffffffffffffffffffffffff")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// Sanity-check the unused context import to keep go vet happy if I drop a usage.
var _ = context.Background
