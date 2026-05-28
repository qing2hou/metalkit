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

func TestIndexPage(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := get(t, ts, "/ui/")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html...", ct)
	}
	if !strings.Contains(body, "metalkit &mdash; 机器列表") {
		t.Errorf("index body missing expected marker; got first 200 bytes: %q", truncate(body, 200))
	}
}

func TestDetailPage(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := get(t, ts, "/ui/m/abc-123")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html...", ct)
	}
	// Marker present in detail.html but not in index.html.
	if !strings.Contains(body, "上报历史") {
		t.Errorf("detail body missing expected marker; got first 200 bytes: %q", truncate(body, 200))
	}
}

func TestImagesPage(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := get(t, ts, "/ui/images")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html...", ct)
	}
	// Marker that's only on the images page.
	if !strings.Contains(body, "上传镜像") {
		t.Errorf("images body missing expected marker; got first 200 bytes: %q", truncate(body, 200))
	}
	if !strings.Contains(body, "/ui/assets/images.js") {
		t.Errorf("images body missing images.js script tag")
	}
}

func TestProfilesPage(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := get(t, ts, "/ui/profiles")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html...", ct)
	}
	if !strings.Contains(body, "安装配置") {
		t.Errorf("profiles body missing expected marker; got first 200 bytes: %q", truncate(body, 200))
	}
	if !strings.Contains(body, "/ui/assets/profiles.js") {
		t.Errorf("profiles body missing profiles.js script tag")
	}
}

func TestSubnetsPage(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := get(t, ts, "/ui/subnets")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html...", ct)
	}
	if !strings.Contains(body, `data-page="subnets"`) {
		t.Errorf("subnets body missing data-page=\"subnets\" marker; got first 200 bytes: %q", truncate(body, 200))
	}
	if !strings.Contains(body, "/ui/assets/subnets.js") {
		t.Errorf("subnets body missing subnets.js script tag")
	}
}

func TestImagesJSAsset(t *testing.T) {
	ts := newTestServer(t)
	status, _, body := get(t, ts, "/ui/assets/images.js")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(body) == 0 {
		t.Errorf("images.js body was empty")
	}
}

func TestLoginPage(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := get(t, ts, "/ui/login")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html...", ct)
	}
	if !strings.Contains(body, `data-page="login"`) {
		t.Errorf("login body missing data-page=\"login\" marker; got first 200 bytes: %q", truncate(body, 200))
	}
	if !strings.Contains(body, "/ui/assets/login.js") {
		t.Errorf("login body missing login.js script tag")
	}
}

func TestLoginJSAsset(t *testing.T) {
	ts := newTestServer(t)
	status, _, body := get(t, ts, "/ui/assets/login.js")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(body) == 0 {
		t.Errorf("login.js body was empty")
	}
}

func TestAppJSAsset(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := get(t, ts, "/ui/assets/app.js")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	// Different stdlib versions report either javascript form; accept both.
	if !strings.HasPrefix(ct, "text/javascript") && !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("content-type = %q, want text/javascript or application/javascript", ct)
	}
	if len(body) == 0 {
		t.Errorf("app.js body was empty")
	}
}

func TestStyleCSSAsset(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := get(t, ts, "/ui/assets/style.css")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type = %q, want text/css...", ct)
	}
	if len(body) == 0 {
		t.Errorf("style.css body was empty")
	}
}

func TestBMCPage(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := get(t, ts, "/ui/bmc")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html...", ct)
	}
	if !strings.Contains(body, `data-page="bmc"`) {
		t.Errorf("bmc body missing data-page=\"bmc\" marker; got first 200 bytes: %q", truncate(body, 200))
	}
	if !strings.Contains(body, "/ui/assets/bmc.js") {
		t.Errorf("bmc body missing bmc.js script tag")
	}
	if !strings.Contains(body, "/ui/assets/common.js") {
		t.Errorf("bmc body missing common.js script tag")
	}
}

func TestJobsPage(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := get(t, ts, "/ui/jobs")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html...", ct)
	}
	// Marker that's only on the jobs list page.
	if !strings.Contains(body, `data-page="jobs"`) {
		t.Errorf("jobs body missing data-page=\"jobs\" marker; got first 200 bytes: %q", truncate(body, 200))
	}
	if !strings.Contains(body, "/ui/assets/jobs.js") {
		t.Errorf("jobs body missing jobs.js script tag")
	}
	if !strings.Contains(body, "/ui/assets/common.js") {
		t.Errorf("jobs body missing common.js script tag")
	}
}

func TestJobDetailPage(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := get(t, ts, "/ui/jobs/deadbeefdeadbeefdeadbeefdeadbeef")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html...", ct)
	}
	// Marker that's only on the job detail page (NOT the jobs list page).
	if !strings.Contains(body, `data-page="job"`) {
		t.Errorf("job detail body missing data-page=\"job\" marker; got first 200 bytes: %q", truncate(body, 200))
	}
	if !strings.Contains(body, "/ui/assets/job.js") {
		t.Errorf("job detail body missing job.js script tag")
	}
	// Make sure the detail handler is distinct from the list handler.
	if strings.Contains(body, `data-page="jobs"`) {
		t.Errorf("job detail body matched the jobs LIST page marker; route wiring wrong")
	}
}

func TestMissingAsset404(t *testing.T) {
	ts := newTestServer(t)
	status, _, _ := get(t, ts, "/ui/assets/missing.txt")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestUnknownPath404(t *testing.T) {
	ts := newTestServer(t)
	status, _, body := get(t, ts, "/ui/unknown")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	// Ensure we didn't accidentally serve the index page.
	if strings.Contains(body, "metalkit &mdash; 机器列表") {
		t.Errorf("/ui/unknown returned the index page body; subtree leak from \"/ui/\"")
	}
}

func TestRootMountRedirect(t *testing.T) {
	ts := newTestServer(t)
	// "/ui" without trailing slash should redirect to "/ui/".
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
