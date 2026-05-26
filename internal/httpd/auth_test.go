package httpd

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"metalkit/internal/inventory"
	"metalkit/internal/sessions"
	"metalkit/internal/sqlitedb"
)

func newAuthTestServer(t *testing.T, adminPass string, withStore bool, withUI bool) *Server {
	t.Helper()
	cfg := Config{
		ListenAddr: ":8080",
		ServerIP:   "10.99.0.1",
		BootDir:    t.TempDir(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminUser:  "admin",
		AdminPass:  adminPass,
	}
	if withStore {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{
			Path:   ":memory:",
			Logger: logger,
		})
		if err != nil {
			t.Fatalf("sqlitedb.Open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		store, err := inventory.NewStore(context.Background(), db, logger)
		if err != nil {
			t.Fatalf("inventory.NewStore: %v", err)
		}
		cfg.Store = store
	}
	if withUI {
		cfg.UI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "ui-body:"+r.URL.Path)
		})
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func basicAuthValue(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func doReq(t *testing.T, ts *httptest.Server, method, path, auth string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func wrapped(s *Server) http.Handler {
	return basicAuth(s.cfg.AdminUser, s.cfg.AdminPass, s.routes())
}

func TestBasicAuthRequiredOnUI(t *testing.T) {
	s := newAuthTestServer(t, "secret", true, true)
	ts := httptest.NewServer(wrapped(s))
	t.Cleanup(ts.Close)

	// No Accept header → middleware treats it as a non-HTML (API) request
	// and returns a JSON 401 rather than redirecting. WWW-Authenticate must
	// NOT be set: we no longer want the browser popup.
	resp := doReq(t, ts, http.MethodGet, "/ui/", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /ui/ (no Accept): status=%d want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate=%q, expected empty (no browser popup)", got)
	}

	resp = doReq(t, ts, http.MethodGet, "/ui/", basicAuthValue("admin", "secret"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authed /ui/: status=%d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ui-body:") {
		t.Errorf("ui body=%q", string(body))
	}
}

func TestBasicAuthRequiredOnMachinesGET(t *testing.T) {
	s := newAuthTestServer(t, "secret", true, false)
	ts := httptest.NewServer(wrapped(s))
	t.Cleanup(ts.Close)

	resp := doReq(t, ts, http.MethodGet, "/api/v1/machines", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/v1/machines: status=%d want 401", resp.StatusCode)
	}

	resp = doReq(t, ts, http.MethodGet, "/api/v1/machines", basicAuthValue("admin", "secret"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authed /api/v1/machines: status=%d want 200", resp.StatusCode)
	}
}

func TestBasicAuthOpenOnAgentEndpoints(t *testing.T) {
	s := newAuthTestServer(t, "secret", true, false)
	ts := httptest.NewServer(wrapped(s))
	t.Cleanup(ts.Close)

	// POST /api/v1/report with empty body — handler will 400, but the
	// auth wrapper must let it through (no 401).
	resp := doReq(t, ts, http.MethodPost, "/api/v1/report", "")
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("POST /api/v1/report: auth wrapper blocked agent endpoint (status=401)")
	}
}

func TestBasicAuthOpenOnHealthz(t *testing.T) {
	s := newAuthTestServer(t, "secret", false, false)
	ts := httptest.NewServer(wrapped(s))
	t.Cleanup(ts.Close)

	resp := doReq(t, ts, http.MethodGet, "/healthz", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz: status=%d want 200", resp.StatusCode)
	}
}

func TestBasicAuthDisabledWhenPassEmpty(t *testing.T) {
	s := newAuthTestServer(t, "", true, true)
	ts := httptest.NewServer(wrapped(s))
	t.Cleanup(ts.Close)

	resp := doReq(t, ts, http.MethodGet, "/ui/", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ui/ with no pass: status=%d want 200 (open mode)", resp.StatusCode)
	}

	resp = doReq(t, ts, http.MethodGet, "/api/v1/machines", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/v1/machines with no pass: status=%d want 200 (open mode)", resp.StatusCode)
	}
}

func TestBasicAuthRejectsWrongPass(t *testing.T) {
	s := newAuthTestServer(t, "secret", true, false)
	ts := httptest.NewServer(wrapped(s))
	t.Cleanup(ts.Close)

	resp := doReq(t, ts, http.MethodGet, "/api/v1/machines", basicAuthValue("admin", "wrong"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong pass: status=%d want 401", resp.StatusCode)
	}
}

func TestNeedsAuthPaths(t *testing.T) {
	cases := map[string]bool{
		"/ui":                                            true,
		"/ui/":                                           true,
		"/ui/m/aaaa-bbbb":                                true,
		"/ui/login":                                      false,
		"/ui/assets/app.js":                              false,
		"/ui/assets/login.js":                            false,
		"/uibad":                                         false, // not a UI subpath
		"/api/v1/machines":                               true,
		"/api/v1/machines/":                              true,
		"/api/v1/machines/abc":                           true,
		"/api/v1/machines/abc/reports":                   true,
		"/api/v1/machines/abc/reports/1":                 true,
		"/api/v1/lookup":                                 true,
		"/api/v1/lookupxxx":                              false,
		"/api/v1/images":                                 true,
		"/api/v1/images/":                                true,
		"/api/v1/images/abc":                             true,
		"/api/v1/images/uploads":                         true,
		"/api/v1/images/uploads/abc":                     true,
		"/api/v1/images/uploads/abc/chunks/1":            true,
		"/api/v1/imagesxxx":                              false,
		"/api/v1/profiles":                               true,
		"/api/v1/profiles/":                              true,
		"/api/v1/profiles/abcd1234abcd1234abcd1234abcd1234": true,
		"/api/v1/profilesxxx":                            false,
		"/api/v1/bindings":                               true,
		"/api/v1/bindings/":                              true,
		"/api/v1/bindings/4c4c4544-0058-3210-8053-c5c04f463830": true,
		"/api/v1/bindingsxxx":                            false,
		"/api/v1/bmc":                                    true,
		"/api/v1/bmc/":                                   true,
		"/api/v1/bmc/4c4c4544-0058-3210-8053-c5c04f463830": true,
		"/api/v1/bmcxxx":                                 false,
		"/api/v1/jobs":                                   true,
		"/api/v1/jobs/":                                  true,
		"/api/v1/jobs/abcd1234abcd1234abcd1234abcd1234": true,
		"/api/v1/jobs/abcd1234abcd1234abcd1234abcd1234/logs": true,
		"/api/v1/jobsxxx":                                false,
		"/api/v1/util":                                   true,
		"/api/v1/util/":                                  true,
		"/api/v1/util/crypt-sha512":                      true,
		"/api/v1/utilxxx":                                false,
		"/api/v1/auth":                                   true,
		"/api/v1/auth/":                                  true,
		"/api/v1/auth/login":                             false,
		"/api/v1/auth/logout":                            false,
		"/api/v1/auth/me":                                true,
		"/api/v1/authxxx":                                false,
		"/api/v1/agent/jobs/current":                     false,
		"/api/v1/agent/jobs/abcd1234abcd1234abcd1234abcd1234/claim":   false,
		"/api/v1/agent/jobs/abcd1234abcd1234abcd1234abcd1234/stage":   false,
		"/api/v1/agent/jobs/abcd1234abcd1234abcd1234abcd1234/logs":    false,
		"/api/v1/agent/jobs/abcd1234abcd1234abcd1234abcd1234/succeed": false,
		"/api/v1/agent/jobs/abcd1234abcd1234abcd1234abcd1234/fail":    false,
		"/api/v1/agent/jobs/abcd1234abcd1234abcd1234abcd1234/spec":    false,
		"/api/v1/agent/images/abcd1234abcd1234abcd1234abcd1234/blob":  false,
		"/api/v1/report":                                 false,
		"/api/v1/heartbeat/abc":                          false,
		"/healthz":                                       false,
		"/boot/vmlinuz":                                  false,
		"/boot/ipxe":                                     false,
		"/":                                              false,
	}
	for p, want := range cases {
		if got := needsAuth(p); got != want {
			t.Errorf("needsAuth(%q)=%v want %v", p, got, want)
		}
	}
}

func TestIPXEScriptIncludesMetalkitURL(t *testing.T) {
	s := newAuthTestServer(t, "", false, false)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	resp := doReq(t, ts, http.MethodGet, "/boot/ipxe", "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "metalkit.url=http://10.99.0.1:8080") {
		t.Errorf("ipxe body missing metalkit.url=: %s", body)
	}
}

// newSessionsStoreForTest returns a fresh sessions.Store backed by an
// in-memory SQLite. Lives in the same db pool as the inventory store from
// newAuthTestServer (separately opened, but each is a private :memory: db,
// which is fine for these auth-only tests).
func newSessionsStoreForTest(t *testing.T) *sessions.Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{
		Path:   ":memory:",
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("sqlitedb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := sessions.NewStore(context.Background(), db, logger)
	if err != nil {
		t.Fatalf("sessions.NewStore: %v", err)
	}
	return store
}

func wrappedWithSessions(s *Server, ss *sessions.Store) http.Handler {
	return sessionOrBasicAuth(s.cfg.AdminUser, s.cfg.AdminPass, ss, s.cfg.Logger, s.routes())
}

func TestSessionCookieAccepted(t *testing.T) {
	s := newAuthTestServer(t, "secret", true, false)
	ss := newSessionsStoreForTest(t)
	sess, err := ss.Create(context.Background(), "admin", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("Sessions.Create: %v", err)
	}
	ts := httptest.NewServer(wrappedWithSessions(s, ss))
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/machines", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie-authed /api/v1/machines: status=%d want 200", resp.StatusCode)
	}
}

func TestSessionCookieUnknownFallsBackTo401(t *testing.T) {
	// Cookie value that's well-formed but not in the store. The middleware
	// gets ErrNotFound from Get and falls through to Basic Auth / 401 — the
	// same code path an ErrExpired result hits.
	s := newAuthTestServer(t, "secret", true, false)
	ss := newSessionsStoreForTest(t)
	ts := httptest.NewServer(wrappedWithSessions(s, ss))
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/machines", nil)
	req.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: strings.Repeat("a", 64), // valid hex shape, no matching row
	})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown cookie /api/v1/machines: status=%d want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate=%q want empty", got)
	}
}

func TestSessionCookieExpiredFallsBackTo401(t *testing.T) {
	// A real expired session: create with a tiny TTL then sleep past it. Yes,
	// it's a wallclock test, but the alternatives (SetClockForTest is internal
	// to the sessions package; direct SQL writes require opening a second
	// handle) are worse for an integration test like this.
	s := newAuthTestServer(t, "secret", true, false)
	ss := newSessionsStoreForTest(t)
	sess, err := ss.Create(context.Background(), "admin", 1*time.Second)
	if err != nil {
		t.Fatalf("Sessions.Create: %v", err)
	}
	// Sleep just over the TTL so Get returns ErrExpired.
	time.Sleep(1100 * time.Millisecond)

	ts := httptest.NewServer(wrappedWithSessions(s, ss))
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/machines", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired-cookie /api/v1/machines: status=%d want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate=%q want empty", got)
	}
}

func TestSessionCookieThenBasicFallback(t *testing.T) {
	// Cookie missing/unknown but valid Basic Auth → should succeed via the
	// Basic Auth branch.
	s := newAuthTestServer(t, "secret", true, false)
	ss := newSessionsStoreForTest(t)
	ts := httptest.NewServer(wrappedWithSessions(s, ss))
	t.Cleanup(ts.Close)

	resp := doReq(t, ts, http.MethodGet, "/api/v1/machines", basicAuthValue("admin", "secret"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Basic Auth fallback /api/v1/machines: status=%d want 200", resp.StatusCode)
	}
}

func TestHTMLUnauthRedirectsToLogin(t *testing.T) {
	s := newAuthTestServer(t, "secret", true, true)
	ts := httptest.NewServer(wrapped(s))
	t.Cleanup(ts.Close)

	// http.Client follows redirects by default; we need to inspect the 302
	// itself, so disable the follower.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/foo", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("HTML /ui/foo unauth: status=%d want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	want := "/ui/login?next=%2Fui%2Ffoo"
	if loc != want {
		t.Errorf("Location=%q want %q", loc, want)
	}
}

func TestHTMLUnauthRedirectViaSecFetchDest(t *testing.T) {
	// Sec-Fetch-Dest: document signals a top-level navigation even without
	// an Accept: text/html — modern browsers always send it.
	s := newAuthTestServer(t, "secret", true, true)
	ts := httptest.NewServer(wrapped(s))
	t.Cleanup(ts.Close)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/", nil)
	req.Header.Set("Sec-Fetch-Dest", "document")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("Sec-Fetch-Dest=document unauth: status=%d want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/ui/login?next=") {
		t.Errorf("Location=%q want /ui/login?next=...", loc)
	}
}

func TestAPIUnauthIsJSON401NoWWWAuthenticate(t *testing.T) {
	s := newAuthTestServer(t, "secret", true, false)
	ts := httptest.NewServer(wrapped(s))
	t.Cleanup(ts.Close)

	resp := doReq(t, ts, http.MethodGet, "/api/v1/machines", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("API unauth: status=%d want 401", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type=%q want application/json", ct)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate=%q want empty (no native browser popup)", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != `{"error":"unauthorized"}` {
		t.Errorf("body=%q want {\"error\":\"unauthorized\"}", strings.TrimSpace(string(body)))
	}
}

func TestUILoginAndAssetsAreOpen(t *testing.T) {
	// /ui/login and /ui/assets/* must reach the UI handler without auth so
	// the login page itself can render.
	s := newAuthTestServer(t, "secret", true, true)
	ts := httptest.NewServer(wrapped(s))
	t.Cleanup(ts.Close)

	for _, path := range []string{"/ui/login", "/ui/assets/app.js", "/ui/assets/login.js"} {
		resp := doReq(t, ts, http.MethodGet, path, "")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s unauth: status=%d want 200; body=%q", path, resp.StatusCode, body)
		}
	}
}

func TestAuthLoginLogoutAreOpen(t *testing.T) {
	// /api/v1/auth/login and /api/v1/auth/logout must NOT be guarded by the
	// middleware — they need to be reachable by an unauthenticated browser.
	// We don't mount the auth API here, so the handlers will 404; that's
	// fine, the test only checks "auth wrapper didn't return 401".
	s := newAuthTestServer(t, "secret", true, false)
	ts := httptest.NewServer(wrapped(s))
	t.Cleanup(ts.Close)

	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/logout"} {
		resp := doReq(t, ts, http.MethodPost, path, "")
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("POST %s: auth wrapper returned 401 — must be open", path)
		}
	}
}
