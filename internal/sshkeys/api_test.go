package sshkeys

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

func postJSON(t *testing.T, ts *httptest.Server, path string, payload any) (*http.Response, string) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func putJSON(t *testing.T, ts *httptest.Server, path string, payload any) (*http.Response, string) {
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
	return resp, string(b)
}

func getJSON(t *testing.T, ts *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func deleteURL(t *testing.T, ts *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestAPICRUDFlow(t *testing.T) {
	_, ts := newTestAPI(t)

	// create → 201 + echo
	resp, body := postJSON(t, ts, "/api/v1/sshkeys",
		map[string]string{"name": "api-key", "public_key": testEd25519Pub})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", resp.StatusCode, body)
	}
	var k SSHKey
	if err := json.Unmarshal([]byte(body), &k); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if k.ID == "" || k.Fingerprint != testEd25519FP {
		t.Fatalf("create response missing id/fingerprint: %s", body)
	}

	// unknown field → 400
	resp, body = postJSON(t, ts, "/api/v1/sshkeys",
		map[string]string{"name": "x", "public_key": testEd25519Pub, "extra": "1"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown field status = %d, body %s", resp.StatusCode, body)
	}

	// invalid key → 400
	resp, body = postJSON(t, ts, "/api/v1/sshkeys",
		map[string]string{"name": "bad", "public_key": "not a key"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid key status = %d, body %s", resp.StatusCode, body)
	}

	// duplicate name → 409
	resp, _ = postJSON(t, ts, "/api/v1/sshkeys",
		map[string]string{"name": "api-key", "public_key": testEd25519Pub})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate name status = %d", resp.StatusCode)
	}

	// list → contains it
	resp, body = getJSON(t, ts, "/api/v1/sshkeys")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, "api-key") {
		t.Errorf("list body missing api-key: %s", body)
	}

	// put rename → 200
	rename := "renamed"
	resp, body = putJSON(t, ts, "/api/v1/sshkeys/"+k.ID,
		map[string]string{"name": rename})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, body %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"renamed"`) {
		t.Errorf("update body missing new name: %s", body)
	}

	// get one
	resp, body = getJSON(t, ts, "/api/v1/sshkeys/"+k.ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, "renamed") {
		t.Errorf("get body missing renamed: %s", body)
	}

	// bad id → 400
	resp, _ = getJSON(t, ts, "/api/v1/sshkeys/zzz")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id status = %d", resp.StatusCode)
	}

	// delete → 204 then 404
	resp, _ = deleteURL(t, ts, "/api/v1/sshkeys/"+k.ID)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d", resp.StatusCode)
	}
	resp, _ = deleteURL(t, ts, "/api/v1/sshkeys/"+k.ID)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second delete status = %d", resp.StatusCode)
	}
}

func TestAPIDeleteRefGuard(t *testing.T) {
	a, ts := newTestAPI(t)
	resp, body := postJSON(t, ts, "/api/v1/sshkeys",
		map[string]string{"name": "guarded", "public_key": testEd25519Pub})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", resp.StatusCode, body)
	}
	var k SSHKey
	_ = json.Unmarshal([]byte(body), &k)

	a.WithBindingRefCount(func(ctx context.Context, keyID string) (int, error) {
		if keyID != k.ID {
			t.Errorf("refcount called with %q, want %q", keyID, k.ID)
		}
		return 3, nil
	})
	resp, body = deleteURL(t, ts, "/api/v1/sshkeys/"+k.ID)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("guarded delete status = %d, body %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "3") {
		t.Errorf("error should mention refcount: %s", body)
	}
}
