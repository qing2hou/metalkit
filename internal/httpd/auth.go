package httpd

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"metalkit/internal/sessions"
	"metalkit/internal/util"
)

// sessionCookieName matches authapi.CookieName. Duplicated here to avoid an
// import cycle (authapi already imports sessions, and httpd will import
// authapi via the Config knob — but the constant itself is part of the
// wire contract, not the API).
const sessionCookieName = "metalkit_session"

// sessionTouchInterval is the minimum gap between sliding-renewal writes per
// session. One hour means a busy admin causes at most one UPDATE per hour
// rather than per request.
const sessionTouchInterval = 1 * time.Hour

// sessionTTL is the lifetime of a fresh or renewed session. Matches the
// CookieTTL the login handler advertises to the browser.
const sessionTTL = 7 * 24 * time.Hour

// sessionOrBasicAuth wraps next so requests matching needsAuth() must present
// either a valid session cookie OR valid HTTP Basic credentials. The browser
// UI uses cookies (minted by /api/v1/auth/login); curl / agents / CI keep
// Basic Auth working.
//
// Credential sources (first match wins):
//  1. session cookie
//  2. legacy AdminUser/AdminPass basic-auth pair (constant-time compare)
//  3. any cfg.Users entry — username constant-time, password verified
//     against the sha512crypt hash via util.VerifyCryptSHA512 (forks
//     mkpasswd; login/basic-auth is a cold path so one fork per attempt
//     is acceptable).
//
// When auth is required but missing:
//   - HTML requests under /ui/*  → 302 to /ui/login?next=<orig>
//   - everything else            → 401 JSON {"error":"unauthorized"}, no
//     WWW-Authenticate header (we don't want the browser popup any more).
//
// If adminPass is empty AND no users are configured the wrapper is open.
func sessionOrBasicAuth(adminUser, adminPass string, users []OperatorUser, sessStore *sessions.Store, logger *slog.Logger, next http.Handler) http.Handler {
	if adminPass == "" && len(users) == 0 {
		return next
	}
	userB := []byte(adminUser)
	passB := []byte(adminPass)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !needsAuth(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// 1. Session cookie. A present-but-expired cookie falls through to
		// Basic Auth (so a stale browser tab doesn't hard-fail an admin who
		// still has working basic creds) and ultimately to the redirect/401.
		if ck, err := r.Cookie(sessionCookieName); err == nil && ck.Value != "" && sessStore != nil {
			sess, err := sessStore.Get(r.Context(), ck.Value)
			if err == nil {
				// Best-effort touch; ignore errors — the request is valid
				// regardless of whether the sliding-renewal write succeeded.
				_, _ = sessStore.Touch(r.Context(), sess.ID, sessionTouchInterval, sessionTTL)
				role := "admin"
				for _, ou := range users {
					if ou.Username == sess.Username {
						role = ou.Role
						break
					}
				}
				ctx := sessions.WithRole(sessions.WithUser(r.Context(), sess.Username), role)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			// ErrNotFound / ErrExpired → fall through.
		}

		// 2 & 3. Basic Auth (curl, CI, agents-with-creds, legacy clients).
		if u, p, ok := r.BasicAuth(); ok {
			if adminPass != "" &&
				subtle.ConstantTimeCompare([]byte(u), userB) == 1 &&
				subtle.ConstantTimeCompare([]byte(p), passB) == 1 {
				next.ServeHTTP(w, r.WithContext(sessions.WithRole(sessions.WithUser(r.Context(), adminUser), "admin")))
				return
			}
			// Named operator accounts. Username is compared constant-time;
			// the password check forks mkpasswd so it's kept out of the
			// constant-time path — timing doesn't leak anything the hash
			// comparison doesn't already.
			for _, ou := range users {
				if subtle.ConstantTimeCompare([]byte(u), []byte(ou.Username)) == 1 {
					ok, err := util.VerifyCryptSHA512(r.Context(), p, ou.PassHash)
					if err != nil {
						logger.Error("verify operator password", "user", ou.Username, "err", err)
					}
					if ok {
						ctx := sessions.WithRole(sessions.WithUser(r.Context(), ou.Username), ou.Role)
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
					// Wrong password for a known user, or verifier error → reject.
					break
				}
			}
		}

		// 4. Reject. HTML top-level navigations get a friendly redirect to the
		// login page; everything else gets a JSON 401 with NO WWW-Authenticate
		// header — we don't want the browser's native Basic Auth popup any
		// more now that we own a login page.
		if isHTMLRequest(r) && strings.HasPrefix(r.URL.Path, "/ui") {
			target := "/ui/login?next=" + url.QueryEscape(r.URL.Path)
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
	})
}

// isHTMLRequest is a best-effort sniff: a browser top-level navigation sends
// Accept: text/html,... ; XHR/fetch defaults to */*. The Sec-Fetch-Dest
// header (modern browsers) is even more reliable when present.
func isHTMLRequest(r *http.Request) bool {
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest == "document" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// needsAuth is fail-closed: everything under /api/v1/ (and the whole /ui
// subtree except login/assets) requires authentication unless it is on the
// explicit open list below. A new route registered without touching this
// function therefore ends up AUTHENTICATED, not exposed — that is the point
// of the default-true design (it used to be default-open: a forgotten entry
// silently published the endpoint).
//
// Open (no auth) by design:
//   - /api/v1/report, /api/v1/heartbeat/* — live-boot inventory agents have
//     no credential store; machine identity is the SMBIOS UUID in the path.
//   - /api/v1/agent/* — the same agents' job/report/blob endpoints. The
//     job/image IDs in the paths are 32-hex unguessables.
//   - /api/v1/auth/login, /api/v1/auth/logout — the login page needs them.
//   - /healthz — load balancers and install checks.
//   - /boot/* — PXE/iPXE clients pull the live system over plain HTTP.
//   - / — the redirect handler to /ui/.
//   - /ui, /ui/* — EXCEPT /ui/login and /ui/assets/*, which must stay open
//     so an unauthenticated browser can render the login page at all.
//
// Regression safety net: TestNeedsAuth_AllRegisteredRoutes walks every route
// the server registers and asserts its intended policy — adding a route
// without deciding its auth status fails the test.
func needsAuth(path string) bool {
	// 1. Always-open paths (exact matches).
	switch path {
	case "/api/v1/report",
		"/api/v1/auth/login",
		"/api/v1/auth/logout",
		"/healthz",
		"/",
		"/ui/login":
		return false
	}
	// 2. Always-open prefixes. Order matters where prefixes overlap.
	for _, p := range []string{
		"/api/v1/heartbeat/", // live agents, UUID in path
		"/api/v1/agent/",     // live agents' job/image endpoints
		"/boot/",             // PXE kernel/initrd/squashfs over plain HTTP
		"/ui/assets/",        // static assets: the login page must render
	} {
		if strings.HasPrefix(path, p) {
			return false
		}
	}
	// 3. The /api/v1 tree and the /ui subtree are closed by default.
	//    (Bare "/api" and "/api/v1" land here too — closed, like before.)
	if strings.HasPrefix(path, "/api/") || path == "/api" ||
		path == "/ui" || strings.HasPrefix(path, "/ui/") {
		return true
	}
	return false
}

// basicAuth is a back-compat shim for tests that pre-date the cookie support.
// New call sites use sessionOrBasicAuth directly; passing nil for the session
// store reduces this wrapper to "Basic Auth only", which is what the legacy
// tests assume.
func basicAuth(adminUser, adminPass string, next http.Handler) http.Handler {
	return sessionOrBasicAuth(adminUser, adminPass, nil, nil, nil, next)
}
