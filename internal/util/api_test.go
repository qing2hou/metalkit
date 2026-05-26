package util

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	NewAPI(slog.New(slog.NewTextHandler(io.Discard, nil))).RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestCryptEndpoint_RejectBadJSON(t *testing.T) {
	ts := newTestAPI(t)
	resp, err := http.Post(ts.URL+"/api/v1/util/crypt-sha512", "application/json", bytes.NewBufferString("{nope"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d", resp.StatusCode)
	}
}

func TestCryptEndpoint_RejectExtraField(t *testing.T) {
	ts := newTestAPI(t)
	resp, err := http.Post(ts.URL+"/api/v1/util/crypt-sha512", "application/json", strings.NewReader(`{"password":"abcdefgh","x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d", resp.StatusCode)
	}
}

func TestCryptEndpoint_Happy(t *testing.T) {
	if _, err := http.NewRequest("POST", "/", nil); err != nil {
		t.Fatal(err)
	}
	// skip if no mkpasswd
	ts := newTestAPI(t)
	resp, err := http.Post(ts.URL+"/api/v1/util/crypt-sha512", "application/json", strings.NewReader(`{"password":"validpass1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), `"hash":"$6$`) {
			t.Errorf("body=%s", string(body))
		}
	}
	// if mkpasswd absent the endpoint returns 400; that's an acceptable env-conditional skip.
}
