package bindings

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestAPI(t *testing.T) (*testFixture, *httptest.Server) {
	t.Helper()
	f := newFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := NewAPI(f.bindings, logger)
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return f, ts
}

func doRequest(t *testing.T, method, url string, payload any) (*http.Response, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, body)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestAPIUpsertHappyPath(t *testing.T) {
	f, ts := newTestAPI(t)
	mu := f.seedMachine(t, '0')
	im := f.seedImage(t, "a")
	pr := f.seedProfile(t, "static-api", "static")

	resp, body := doRequest(t, http.MethodPut, ts.URL+"/api/v1/bindings/"+mu, map[string]any{
		"image_id":       im,
		"profile_id":     pr,
		"desired_state":  "install",
		"static_address": "10.0.0.7",
		"hostname":       "node7",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var b Binding
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.MachineUUID != mu || b.ImageID != im || b.ProfileID != pr {
		t.Errorf("refs: %+v", b)
	}

	// GET roundtrips.
	resp2, body2 := doRequest(t, http.MethodGet, ts.URL+"/api/v1/bindings/"+mu, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", resp2.StatusCode, body2)
	}
}

func TestAPIUpsertUnknownMachine(t *testing.T) {
	f, ts := newTestAPI(t)
	im := f.seedImage(t, "b")
	pr := f.seedProfile(t, "p1", "dhcp")
	resp, _ := doRequest(t, http.MethodPut,
		ts.URL+"/api/v1/bindings/ffffffff-ffff-ffff-ffff-ffffffffffff",
		map[string]any{
			"image_id": im, "profile_id": pr, "desired_state": "install",
		})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func TestAPIUpsertUnknownImage(t *testing.T) {
	f, ts := newTestAPI(t)
	mu := f.seedMachine(t, '1')
	pr := f.seedProfile(t, "p2", "dhcp")
	resp, _ := doRequest(t, http.MethodPut, ts.URL+"/api/v1/bindings/"+mu, map[string]any{
		"image_id": "00000000000000000000000000000000",
		"profile_id": pr, "desired_state": "install",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func TestAPIUpsertBadRequest(t *testing.T) {
	f, ts := newTestAPI(t)
	mu := f.seedMachine(t, '2')
	im := f.seedImage(t, "c")
	pr := f.seedProfile(t, "p3", "static")
	// missing static_address for a static profile → 400 from store validate.
	resp, _ := doRequest(t, http.MethodPut, ts.URL+"/api/v1/bindings/"+mu, map[string]any{
		"image_id": im, "profile_id": pr, "desired_state": "install",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAPIUpsertUnknownField(t *testing.T) {
	f, ts := newTestAPI(t)
	mu := f.seedMachine(t, '3')
	im := f.seedImage(t, "d")
	pr := f.seedProfile(t, "p4", "dhcp")
	resp, _ := doRequest(t, http.MethodPut, ts.URL+"/api/v1/bindings/"+mu, map[string]any{
		"image_id": im, "profile_id": pr, "desired_state": "install",
		"surprise": "extra",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown field", resp.StatusCode)
	}
}

func TestAPIList(t *testing.T) {
	f, ts := newTestAPI(t)
	mu1 := f.seedMachine(t, '4')
	mu2 := f.seedMachine(t, '5')
	im := f.seedImage(t, "e")
	pr := f.seedProfile(t, "p-list", "dhcp")
	for _, mu := range []string{mu1, mu2} {
		resp, _ := doRequest(t, http.MethodPut, ts.URL+"/api/v1/bindings/"+mu, map[string]any{
			"image_id": im, "profile_id": pr, "desired_state": "install",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("seed PUT %s: %d", mu, resp.StatusCode)
		}
	}
	resp, body := doRequest(t, http.MethodGet, ts.URL+"/api/v1/bindings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	var got []Binding
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(got)=%d, want 2", len(got))
	}
}

func TestAPIDelete(t *testing.T) {
	f, ts := newTestAPI(t)
	mu := f.seedMachine(t, '6')
	im := f.seedImage(t, "f")
	pr := f.seedProfile(t, "p-del", "dhcp")
	doRequest(t, http.MethodPut, ts.URL+"/api/v1/bindings/"+mu, map[string]any{
		"image_id": im, "profile_id": pr, "desired_state": "install",
	})
	resp, _ := doRequest(t, http.MethodDelete, ts.URL+"/api/v1/bindings/"+mu, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp2, _ := doRequest(t, http.MethodGet, ts.URL+"/api/v1/bindings/"+mu, nil)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete: %d, want 404", resp2.StatusCode)
	}
}

func TestAPIInvalidUUID(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, _ := doRequest(t, http.MethodGet, ts.URL+"/api/v1/bindings/not-a-uuid", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
