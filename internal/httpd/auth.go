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
// When auth is required but missing:
//   - HTML requests under /ui/*  → 302 to /ui/login?next=<orig>
//   - everything else            → 401 JSON {"error":"unauthorized"}, no
//     WWW-Authenticate header (we don't want the browser popup any more).
//
// If adminPass is empty the wrapper is open (same as before).
func sessionOrBasicAuth(adminUser, adminPass string, sessStore *sessions.Store, logger *slog.Logger, next http.Handler) http.Handler {
	if adminPass == "" {
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
				next.ServeHTTP(w, r.WithContext(sessions.WithUser(r.Context(), sess.Username)))
				return
			}
			// ErrNotFound / ErrExpired → fall through.
		}

		// 2. Basic Auth (curl, CI, agents-with-creds, legacy clients).
		if u, p, ok := r.BasicAuth(); ok &&
			subtle.ConstantTimeCompare([]byte(u), userB) == 1 &&
			subtle.ConstantTimeCompare([]byte(p), passB) == 1 {
			next.ServeHTTP(w, r.WithContext(sessions.WithUser(r.Context(), adminUser)))
			return
		}

		// 3. Reject. HTML top-level navigations get a friendly redirect to the
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

// needsAuth returns true for paths that must be authenticated:
//   - the Web UI (/ui, /ui/*) — except /ui/login and /ui/assets/* which must
//     remain reachable without auth so the login page itself can render.
//   - the read side of the inventory API (/api/v1/machines*, /api/v1/lookup)
//   - the entire images API (/api/v1/images*) — uploads, listing, delete; agents
//     do not upload images, so the whole subtree is admin-only.
//   - the entire profiles API (/api/v1/profiles*) — install templates; admins
//     edit these, agents only read them indirectly via jobs.
//   - the entire bindings API (/api/v1/bindings*) — per-machine install
//     assignments; admins only.
//   - the entire BMC credentials API (/api/v1/bmc*) — IPMI credentials; admins
//     only. Passwords never appear in GET responses (encrypted at rest, returned
//     plaintext only to the in-process ipmitool wrapper).
//   - the operator side of the jobs API (/api/v1/jobs*) — read/cancel install
//     jobs. The agent-facing endpoints (/api/v1/agent/jobs/*) stay open / use
//     token auth (M2.3-6).
//   - the util API (/api/v1/util*) — admin-only helpers like SHA-512 crypt;
//     never exposed to live-boot agents.
//   - the auth me endpoint (/api/v1/auth/me) — a who-am-I echo; login and
//     logout are open so unauthenticated browsers can reach them.
//
// Agent POST endpoints (/api/v1/report, /api/v1/heartbeat/*) stay open because
// live-boot agents have no credential store. /healthz and /boot/* are open so
// PXE clients and load balancers can reach them.
func needsAuth(path string) bool {
	// Explicit overrides — these MUST come before the /ui prefix match so
	// /ui/login (open) wins over the generic /ui/* (closed).
	switch path {
	case "/ui/login":
		return false
	case "/api/v1/auth/login", "/api/v1/auth/logout":
		return false
	}
	if strings.HasPrefix(path, "/ui/assets/") {
		return false
	}
	// Any other /api/v1/auth* (currently just /me, plus future siblings)
	// requires auth.
	if path == "/api/v1/auth" || strings.HasPrefix(path, "/api/v1/auth/") {
		return true
	}

	switch {
	case path == "/ui", strings.HasPrefix(path, "/ui/"):
		return true
	case path == "/api/v1/machines", strings.HasPrefix(path, "/api/v1/machines/"):
		return true
	case path == "/api/v1/lookup":
		return true
	case path == "/api/v1/images", strings.HasPrefix(path, "/api/v1/images/"):
		return true
	case path == "/api/v1/profiles", strings.HasPrefix(path, "/api/v1/profiles/"):
		return true
	case path == "/api/v1/bindings", strings.HasPrefix(path, "/api/v1/bindings/"):
		return true
	case path == "/api/v1/bmc", strings.HasPrefix(path, "/api/v1/bmc/"):
		return true
	case path == "/api/v1/jobs", strings.HasPrefix(path, "/api/v1/jobs/"):
		return true
	case path == "/api/v1/util", strings.HasPrefix(path, "/api/v1/util/"):
		return true
	}
	return false
}

// basicAuth is a back-compat shim for tests that pre-date the cookie support.
// New call sites use sessionOrBasicAuth directly; passing nil for the session
// store reduces this wrapper to "Basic Auth only", which is what the legacy
// tests assume.
func basicAuth(adminUser, adminPass string, next http.Handler) http.Handler {
	return sessionOrBasicAuth(adminUser, adminPass, nil, nil, next)
}
