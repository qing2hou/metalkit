package subnets

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

func apiValidPayload(name string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": "via api",
		"cidr":        "192.168.10.0/24",
		"gateway":     "192.168.10.1",
		"dns":         []string{"1.1.1.1"},
	}
}

func TestAPICreateThenGet(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, body := postJSON(t, ts, "/api/v1/subnets", apiValidPayload("api1"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var sn Subnet
	if err := json.Unmarshal(body, &sn); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !subnetIDRE.MatchString(sn.ID) {
		t.Errorf("id %q not 32 hex", sn.ID)
	}

	resp2, body2 := getJSON(t, ts, "/api/v1/subnets/"+sn.ID)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", resp2.StatusCode, body2)
	}
	var got Subnet
	_ = json.Unmarshal(body2, &got)
	if got.Name != sn.Name || got.CIDR != "192.168.10.0/24" {
		t.Errorf("get mismatch: %+v", got)
	}
}

func TestAPICreateBadJSON(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, err := http.Post(ts.URL+"/api/v1/subnets", "application/json", strings.NewReader("not json"))
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
	resp, _ := postJSON(t, ts, "/api/v1/subnets", p)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown field", resp.StatusCode)
	}
}

func TestAPICreateDuplicateName(t *testing.T) {
	_, ts := newTestAPI(t)
	postJSON(t, ts, "/api/v1/subnets", apiValidPayload("dup-api"))
	resp, _ := postJSON(t, ts, "/api/v1/subnets", apiValidPayload("dup-api"))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestAPIList(t *testing.T) {
	_, ts := newTestAPI(t)
	for _, n := range []string{"l1", "l2"} {
		postJSON(t, ts, "/api/v1/subnets", apiValidPayload(n))
	}
	resp, body := getJSON(t, ts, "/api/v1/subnets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	var got []Subnet
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2", len(got))
	}
}

func TestAPIUpdate(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, body := postJSON(t, ts, "/api/v1/subnets", apiValidPayload("upd1"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", resp.StatusCode, body)
	}
	var sn Subnet
	_ = json.Unmarshal(body, &sn)
	resp2, body2 := putJSON(t, ts, "/api/v1/subnets/"+sn.ID,
		map[string]any{"description": "new desc"})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", resp2.StatusCode, body2)
	}
	var updated Subnet
	_ = json.Unmarshal(body2, &updated)
	if updated.Description != "new desc" {
		t.Errorf("description = %q, want %q", updated.Description, "new desc")
	}
	if updated.CIDR != sn.CIDR {
		t.Errorf("cidr should be unchanged: was %q now %q", sn.CIDR, updated.CIDR)
	}
}

func TestAPIUpdateRejectsBadValue(t *testing.T) {
	_, ts := newTestAPI(t)
	_, body := postJSON(t, ts, "/api/v1/subnets", apiValidPayload("upd2"))
	var sn Subnet
	_ = json.Unmarshal(body, &sn)
	resp, _ := putJSON(t, ts, "/api/v1/subnets/"+sn.ID,
		map[string]any{"gateway": "10.0.0.1"}) // outside existing CIDR
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAPIUpdateNotFound(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, _ := putJSON(t, ts, "/api/v1/subnets/00000000000000000000000000000000",
		map[string]any{"description": "x"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAPIDelete(t *testing.T) {
	_, ts := newTestAPI(t)
	_, body := postJSON(t, ts, "/api/v1/subnets", apiValidPayload("del1"))
	var sn Subnet
	_ = json.Unmarshal(body, &sn)

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/subnets/"+sn.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	resp2, _ := getJSON(t, ts, "/api/v1/subnets/"+sn.ID)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("GET after DELETE: status = %d, want 404", resp2.StatusCode)
	}
}

func TestAPIDeleteRefusedWhenInUse(t *testing.T) {
	a, ts := newTestAPI(t)
	_, body := postJSON(t, ts, "/api/v1/subnets", apiValidPayload("del-inuse"))
	var sn Subnet
	_ = json.Unmarshal(body, &sn)

	// Inject a ref-counter that reports the subnet is referenced.
	a.WithBindingRefCount(func(_ context.Context, id string) (int, error) {
		if id == sn.ID {
			return 3, nil
		}
		return 0, nil
	})

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/subnets/"+sn.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "in use by 3") {
		t.Errorf("body %q does not mention ref count", string(b))
	}

	// Subnet still exists.
	resp2, _ := getJSON(t, ts, "/api/v1/subnets/"+sn.ID)
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("subnet vanished: GET status = %d", resp2.StatusCode)
	}
}

func TestAPIInvalidID(t *testing.T) {
	_, ts := newTestAPI(t)
	cases := []string{
		"/api/v1/subnets/short",
		"/api/v1/subnets/00000000000000000000000000000000zzzz",
		"/api/v1/subnets/notHEX_NOT_HEX_NOT_HEX_NOT_HEX_XX",
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
	body, _ := json.Marshal(apiValidPayload("auth1"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subnets", bytes.NewReader(body))
	req.SetBasicAuth("alice", "secret")
	w := httptest.NewRecorder()
	a.create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var sn Subnet
	_ = json.Unmarshal(w.Body.Bytes(), &sn)
	if sn.CreatedBy != "alice" {
		t.Errorf("created_by = %q, want %q", sn.CreatedBy, "alice")
	}
}

func TestAPIGetMissing(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, _ := getJSON(t, ts, "/api/v1/subnets/ffffffffffffffffffffffffffffffff")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
