package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(Handler(Config{Mount: "/ui"}))
	t.Cleanup(ts.Close)
	return ts
}

// get does a GET against the test server and returns status, content-type and body.
func get(t *testing.T, ts *httptest.Server, path string) (int, string, string) {
	t.Helper()
	// Don't follow redirects — some tests want to inspect them directly.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body %s: %v", path, err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

// TestSpaShell verifies the index path serves the Vite-built SPA shell with
// the entry script referenced under /ui/assets/.
func TestSpaShell(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := get(t, ts, "/ui/")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html...", ct)
	}
	if !strings.Contains(body, `<div id="app">`) {
		t.Errorf("index body missing #app mount point; got first 200 bytes: %q", truncate(body, 200))
	}
	if !strings.Contains(body, "/ui/assets/") {
		t.Errorf("index body missing hashed asset reference")
	}
}

// TestSpaFallback verifies deep links (client-side routes such as
// /ui/m/{uuid} or /ui/jobs/{id}) fall back to the SPA shell so the Vue
// router can pick them up on a fresh page load.
func TestSpaFallback(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{"/ui/login", "/ui/m/abc-123", "/ui/jobs/deadbeef", "/ui/images", "/ui/profiles", "/ui/subnets", "/ui/bmc", "/ui/jobs", "/ui/settings"} {
		status, ct, body := get(t, ts, path)
		if status != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, status)
			continue
		}
		if !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s: content-type = %q, want text/html...", path, ct)
		}
		if !strings.Contains(body, `<div id="app">`) {
			t.Errorf("GET %s: body missing SPA shell marker", path)
		}
	}
}

// TestHashedAssetServed verifies a real file from the Vite build output is
// served under /ui/assets/ (the prefix the httpd auth whitelist allows).
func TestHashedAssetServed(t *testing.T) {
	ts := newTestServer(t)
	// Discover the hashed entry from the shell to avoid hard-coding a hash.
	_, _, body := get(t, ts, "/ui/")
	marker := "/ui/assets/"
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatalf("index body has no /ui/assets/ reference")
	}
	rest := body[idx+len(marker):]
	end := strings.IndexAny(rest, `"'`)
	if end < 0 {
		t.Fatalf("could not parse asset name from index body")
	}
	asset := marker + rest[:end]

	status, ct, assetBody := get(t, ts, asset)
	if status != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", asset, status)
	}
	if len(assetBody) == 0 {
		t.Errorf("GET %s: body was empty", asset)
	}
	if !strings.Contains(ct, "javascript") && !strings.Contains(ct, "css") {
		t.Logf("GET %s content-type = %q (informational)", asset, ct)
	}
}

func TestMissingAsset404(t *testing.T) {
	ts := newTestServer(t)
	status, _, _ := get(t, ts, "/ui/assets/missing.txt")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

// TestRootMountRedirect verifies /ui redirects to /ui/.
func TestRootMountRedirect(t *testing.T) {
	ts := newTestServer(t)
	status, _, _ := get(t, ts, "/ui")
	if status != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", status)
	}
}

func TestDefaultMount(t *testing.T) {
	// Empty Mount defaults to /ui.
	ts := httptest.NewServer(Handler(Config{}))
	t.Cleanup(ts.Close)
	status, _, _ := get(t, ts, "/ui/")
	if status != http.StatusOK {
		t.Fatalf("default mount /ui/ status = %d, want 200", status)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
