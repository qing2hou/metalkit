package authapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"metalkit/internal/sessions"
	"metalkit/internal/sqlitedb"
)

func newTestStore(t *testing.T) *sessions.Store {
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

func newTestAPI(t *testing.T, pass string) *API {
	t.Helper()
	return &API{
		Sessions:   newTestStore(t),
		AdminUser:  "admin",
		AdminPass:  pass,
		CookieTTL:  7 * 24 * time.Hour,
		SecureFlag: false,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func postJSON(t *testing.T, h http.HandlerFunc, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Result()
}

func TestLoginOKSetsCookieAndBody(t *testing.T) {
	a := newTestAPI(t, "secret")
	resp := postJSON(t, a.login, `{"username":"admin","password":"secret"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	body := strings.TrimSpace(string(bodyBytes))
	if body != `{"username":"admin"}` {
		t.Errorf("body=%q want {\"username\":\"admin\"}", body)
	}

	var sc *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			sc = c
			break
		}
	}
	if sc == nil {
		t.Fatalf("no metalkit_session cookie in response; got cookies=%v", resp.Cookies())
	}

	// 64-char lowercase hex
	if len(sc.Value) != 64 {
		t.Errorf("cookie value len=%d want 64", len(sc.Value))
	}
	for i := 0; i < len(sc.Value); i++ {
		c := sc.Value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("cookie value not lowercase hex: %q (char %d=%q)", sc.Value, i, c)
			break
		}
	}

	if !sc.HttpOnly {
		t.Errorf("cookie HttpOnly=false want true")
	}
	if sc.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie SameSite=%v want Strict", sc.SameSite)
	}
	if sc.Path != "/" {
		t.Errorf("cookie Path=%q want /", sc.Path)
	}
	if sc.MaxAge != int(7*24*time.Hour/time.Second) {
		t.Errorf("cookie MaxAge=%d want %d", sc.MaxAge, int(7*24*time.Hour/time.Second))
	}
	if sc.Secure {
		t.Errorf("cookie Secure=true; expected false with SecureFlag=false")
	}

	// Verify the session row really exists.
	if _, err := a.Sessions.Get(context.Background(), sc.Value); err != nil {
		t.Errorf("Sessions.Get(%q) err=%v want nil (cookie should point to a real session)", sc.Value, err)
	}
}

func TestLoginWrongCredentials(t *testing.T) {
	a := newTestAPI(t, "secret")
	cases := []struct {
		name string
		body string
	}{
		{"wrong user", `{"username":"root","password":"secret"}`},
		{"wrong pass", `{"username":"admin","password":"nope"}`},
		{"both wrong", `{"username":"root","password":"nope"}`},
		{"empty username", `{"username":"","password":"secret"}`},
		{"empty password", `{"username":"admin","password":""}`},
		{"empty body", ``},
		{"malformed json", `{"username":"admin",`},
		{"unknown field", `{"username":"admin","password":"secret","extra":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postJSON(t, a.login, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status=%d want 401", resp.StatusCode)
			}
			for _, c := range resp.Cookies() {
				if c.Name == CookieName {
					t.Errorf("unexpected Set-Cookie on failed login: %+v", c)
				}
			}
			bodyBytes, _ := io.ReadAll(resp.Body)
			body := strings.TrimSpace(string(bodyBytes))
			if body != `{"error":"invalid credentials"}` {
				t.Errorf("body=%q want {\"error\":\"invalid credentials\"}", body)
			}
		})
	}
}

func TestLoginRefusedWhenAuthDisabled(t *testing.T) {
	a := newTestAPI(t, "") // empty AdminPass
	resp := postJSON(t, a.login, `{"username":"admin","password":"anything"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", resp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := strings.TrimSpace(string(bodyBytes))
	if body != `{"error":"auth disabled"}` {
		t.Errorf("body=%q want {\"error\":\"auth disabled\"}", body)
	}
}

func TestLogoutWithValidCookieDeletesSession(t *testing.T) {
	a := newTestAPI(t, "secret")
	sess, err := a.Sessions.Create(context.Background(), "admin", a.CookieTTL)
	if err != nil {
		t.Fatalf("Sessions.Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	a.logout(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d want 204", resp.StatusCode)
	}

	var sc *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			sc = c
			break
		}
	}
	if sc == nil {
		t.Fatalf("no metalkit_session cookie in logout response")
	}
	if sc.Value != "" {
		t.Errorf("logout cookie value=%q want empty", sc.Value)
	}
	// Go normalizes MaxAge: -1 → serialized "Max-Age=0", parsed back as
	// MaxAge=-1. Either form indicates deletion.
	if sc.MaxAge >= 0 {
		// Inspect raw header as a fallback assertion.
		raw := resp.Header.Get("Set-Cookie")
		if !strings.Contains(raw, "Max-Age=0") {
			t.Errorf("Set-Cookie=%q, want Max-Age=0 (deletion)", raw)
		}
	}

	// The session row should be gone.
	if _, err := a.Sessions.Get(context.Background(), sess.ID); !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("Sessions.Get after logout err=%v want ErrNotFound", err)
	}
}

func TestLogoutWithoutCookieStillClears(t *testing.T) {
	a := newTestAPI(t, "secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	rec := httptest.NewRecorder()
	a.logout(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d want 204", resp.StatusCode)
	}
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			found = true
			if c.Value != "" {
				t.Errorf("logout cookie value=%q want empty", c.Value)
			}
		}
	}
	if !found {
		t.Errorf("logout did not set the deletion cookie")
	}
}

func TestMeNoContextUserReturns401(t *testing.T) {
	a := newTestAPI(t, "secret")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	rec := httptest.NewRecorder()
	a.me(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", resp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := strings.TrimSpace(string(bodyBytes))
	if body != `{"error":"unauthorized"}` {
		t.Errorf("body=%q want {\"error\":\"unauthorized\"}", body)
	}
}

func TestMeWithContextUserReturnsUsername(t *testing.T) {
	a := newTestAPI(t, "secret")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req = req.WithContext(sessions.WithUser(req.Context(), "admin"))
	rec := httptest.NewRecorder()
	a.me(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := strings.TrimSpace(string(bodyBytes))
	if body != `{"username":"admin"}` {
		t.Errorf("body=%q want {\"username\":\"admin\"}", body)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type=%q want application/json", ct)
	}
}

func TestRegisterRoutesMountsEndpoints(t *testing.T) {
	a := newTestAPI(t, "secret")
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"secret"}`))
	if err != nil {
		t.Fatalf("Post login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login via mux: status=%d want 200", resp.StatusCode)
	}
}
