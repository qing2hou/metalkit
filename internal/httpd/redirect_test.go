package httpd

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMachineChannelOrRedirect(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_ = logger

	served := ""
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	h := machineChannelOrRedirect(":8443", next)

	cases := []struct {
		path      string
		wantServe bool // machine channel: served verbatim
		wantLoc   string
	}{
		{"/healthz", true, ""},
		{"/boot/ipxe", true, ""},
		{"/boot/filesystem.squashfs", true, ""},
		{"/api/v1/report", true, ""},
		{"/api/v1/heartbeat/abc", true, ""},
		{"/api/v1/agent/jobs/current", true, ""},
		{"/ui/", false, "https://ctrl.example:8443/ui/"},
		{"/api/v1/machines", false, "https://ctrl.example:8443/api/v1/machines"},
		{"/api/v1/subnets", false, "https://ctrl.example:8443/api/v1/subnets"},
		{"/api/v1/auth/login", false, "https://ctrl.example:8443/api/v1/auth/login"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Host = "ctrl.example:8080"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if tc.wantServe {
				if rec.Code != http.StatusOK || served != tc.path {
					t.Errorf("machine path %s: status=%d served=%q", tc.path, rec.Code, served)
				}
				return
			}
			loc := rec.Header().Get("Location")
			if rec.Code != http.StatusPermanentRedirect {
				t.Errorf("human path %s: status=%d, want 308", tc.path, rec.Code)
			}
			if loc != tc.wantLoc {
				t.Errorf("human path %s: Location=%q, want %q", tc.path, loc, tc.wantLoc)
			}
		})
	}
}

// POST must stay POST across the redirect (308 semantics) — API clients
// pointed at http:// by accident should transparently retry over TLS.
func TestRedirectPreservesMethod(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("should not reach handler")
	})
	h := machineChannelOrRedirect(":8443", next)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subnets", nil)
	req.Host = "10.1.2.3:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "https://10.1.2.3:8443/api/v1/subnets" {
		t.Errorf("Location = %q", got)
	}
}
