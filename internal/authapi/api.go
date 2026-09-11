// Package authapi serves /api/v1/auth/{login,logout,me} on behalf of the
// browser UI. It does NOT do session validation for protected endpoints —
// that's the auth middleware in internal/httpd. Login mints a fresh
// metalkit_session cookie; logout deletes the session row and clears the
// cookie; me echoes back the username attached to the request context by
// the middleware.
package authapi

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"metalkit/internal/sessions"
	"metalkit/internal/util"
)

// CookieName is the browser session cookie name. The auth middleware in
// internal/httpd reads the same constant value.
const CookieName = "metalkit_session"

// maxLoginBody caps the login request body. Username/password are short; a
// 4 KB limit kills accidental floods without rejecting any legitimate input.
const maxLoginBody = 4 * 1024

// API mounts the auth endpoints. AdminUser/AdminPass are the legacy static
// credential pair; Users are named operator accounts (username + sha512crypt
// hash). At least one credential source must be configured for login to be
// enabled. SecureFlag toggles the cookie's Secure attribute (off on plain
// HTTP, on when serving HTTPS).
type API struct {
	Sessions   *sessions.Store
	AdminUser  string
	AdminPass  string
	Users      []Operator
	CookieTTL  time.Duration
	SecureFlag bool
	Logger     *slog.Logger

	// throttle blunts per-IP brute force on the login endpoint (and the
	// Basic-Auth path shares the per-IP counter via httpd's own instance —
	// this one only guards JSON login).
	throttle *loginThrottle
}

// Operator is one named operator account accepted by login / basic auth.
type Operator struct {
	Username string
	PassHash string // $6$ sha512crypt
	Role     string // admin | operator
}

// RegisterRoutes attaches the auth endpoints to mux.
func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/login", a.login)
	mux.HandleFunc("POST /api/v1/auth/logout", a.logout)
	mux.HandleFunc("GET /api/v1/auth/me", a.me)
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Username string `json:"username"`
}

// dummyVerifyHash is a syntactically valid sha512crypt hash of an
// unguessable random value. When an unknown username is submitted we run a
// verification against it anyway, so the response time is dominated by the
// same mkpasswd fork whether or not the account exists — closing the
// username-enumeration timing side channel.
const dummyVerifyHash = `$6$fixedsalt123$hWfVjXzu1Ia7IE9o2bqMSxIyjTx4TaD.rLuSo11.BVcWwbXQxjbmVtroursOVAjIofVa2p1bhJjq4DdGRYu9X/`

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	if a.AdminPass == "" && len(a.Users) == 0 {
		// No credentials configured → no login flow. The middleware's "open
		// mode" already lets unauthenticated requests through, so a cookie
		// buys nothing. Surface the misconfiguration loudly rather than
		// silently minting useless sessions.
		writeError(w, http.StatusServiceUnavailable, "auth disabled")
		return
	}

	if a.throttle == nil {
		// Wire-up safety: tests construct &API{} literals. 10 fails / 5 min
		// matches the documented operator guidance.
		a.throttle = newLoginThrottle(10, 5*time.Minute)
	}
	if a.throttle.blocked(r) {
		a.logFailure(strings.TrimSpace(""))
		writeError(w, http.StatusTooManyRequests, "too many failed attempts; retry later")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
	var in loginRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		a.logFailure("")
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	username := strings.TrimSpace(in.Username)
	password := in.Password

	authenticated := ""
	// Legacy static pair: compare both fields unconditionally and combine.
	if a.AdminPass != "" {
		userOK := subtle.ConstantTimeCompare([]byte(username), []byte(a.AdminUser))
		passOK := subtle.ConstantTimeCompare([]byte(password), []byte(a.AdminPass))
		if username != "" && password != "" && userOK == 1 && passOK == 1 {
			authenticated = a.AdminUser
		}
	}
	// Named operator accounts: sha512crypt verification (forks mkpasswd;
	// login is a cold path).
	if authenticated == "" {
		matched := false
		for _, u := range a.Users {
			if subtle.ConstantTimeCompare([]byte(username), []byte(u.Username)) != 1 {
				continue
			}
			matched = true
			ok, err := util.VerifyCryptSHA512(r.Context(), password, u.PassHash)
			if err != nil && a.Logger != nil {
				a.Logger.Error("auth login: verify", "user", u.Username, "err", err)
			}
			if ok {
				authenticated = u.Username
			}
			break // a username matches at most one account; wrong pass → reject
		}
		// Unknown username: burn the same mkpasswd fork against a dummy hash
		// so the 401 latency can't distinguish "no such user" (fast) from
		// "wrong password" (fork, slow).
		if !matched {
			_, _ = util.VerifyCryptSHA512(r.Context(), password, dummyVerifyHash)
		}
	}

	if authenticated == "" {
		a.throttle.noteFailure(r)
		a.logFailure(username)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	a.throttle.noteSuccess(r)

	sess, err := a.Sessions.Create(r.Context(), authenticated, a.CookieTTL)
	if err != nil {
		if a.Logger != nil {
			a.Logger.Error("auth login: session create failed", "err", err)
		}
		writeError(w, http.StatusInternalServerError, "session create failed")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   a.SecureFlag,
		MaxAge:   int(a.CookieTTL / time.Second),
	})

	if a.Logger != nil {
		// session_id_prefix is enough to correlate with the audit log without
		// leaking the full token (which is bearer-equivalent).
		a.Logger.Info("auth login ok", "username", authenticated, "session_id_prefix", sess.ID[:8])
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(loginResponse{Username: authenticated})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(CookieName); err == nil && ck.Value != "" && a.Sessions != nil {
		// Idempotent — Delete swallows missing rows and malformed IDs.
		_ = a.Sessions.Delete(r.Context(), ck.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   a.SecureFlag,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

type meResponse struct {
	Username string `json:"username"`
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	username := sessions.UserFromContext(r.Context())
	if username == "" {
		// Defensive — the middleware should have rejected this request before
		// it reached us. Mirror the JSON shape so the frontend sees the same
		// error format regardless of which layer enforced the check.
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(meResponse{Username: username})
}

func (a *API) logFailure(username string) {
	if a.Logger == nil {
		return
	}
	// Log the submitted username (possibly empty) but never the password.
	a.Logger.Warn("auth login failed", "username", username)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
