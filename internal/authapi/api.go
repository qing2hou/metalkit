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
)

// CookieName is the browser session cookie name. The auth middleware in
// internal/httpd reads the same constant value.
const CookieName = "metalkit_session"

// maxLoginBody caps the login request body. Username/password are short; a
// 4 KB limit kills accidental floods without rejecting any legitimate input.
const maxLoginBody = 4 * 1024

// API mounts the auth endpoints. AdminUser/AdminPass are the single static
// credential pair the controller compares against; SecureFlag toggles the
// cookie's Secure attribute (off on plain HTTP, on once we serve HTTPS).
type API struct {
	Sessions   *sessions.Store
	AdminUser  string
	AdminPass  string
	CookieTTL  time.Duration
	SecureFlag bool
	Logger     *slog.Logger
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

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	if a.AdminPass == "" {
		// No password configured → no login flow. The middleware's "open mode"
		// already lets unauthenticated requests through, so a cookie buys
		// nothing. Surface the misconfiguration loudly rather than silently
		// minting useless sessions.
		writeError(w, http.StatusServiceUnavailable, "auth disabled")
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

	// Compare both fields unconditionally and combine — branching on the
	// first mismatch would leak which field was wrong via timing. We still
	// short-circuit the "empty field" case with the same 401 so the client
	// can't tell empty from wrong either.
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(a.AdminUser))
	passOK := subtle.ConstantTimeCompare([]byte(password), []byte(a.AdminPass))
	if username == "" || password == "" || userOK != 1 || passOK != 1 {
		a.logFailure(username)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	sess, err := a.Sessions.Create(r.Context(), a.AdminUser, a.CookieTTL)
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
		a.Logger.Info("auth login ok", "username", a.AdminUser, "session_id_prefix", sess.ID[:8])
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(loginResponse{Username: a.AdminUser})
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
