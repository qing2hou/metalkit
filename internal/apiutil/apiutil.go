// Package apiutil holds the cross-cutting HTTP-API helpers every internal
// API package needs: JSON responses, errors, and request-actor resolution.
// Until now each package carried its own byte-identical copy of writeJSON /
// writeError / basicAuthUser — 12 copies that had started to drift.
package apiutil

import (
	"encoding/json"
	"net/http"

	"metalkit/internal/sessions"
)

// WriteJSON writes v as a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes {"error": msg} with the given status code.
func WriteError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// RequestActor names who is performing a request, for audit rows and
// updated_by / uploaded_by columns:
//
//  1. the session-authenticated username (set by httpd's sessionOrBasicAuth
//     from the login cookie) when present;
//  2. otherwise the HTTP Basic username;
//  3. otherwise "anonymous".
//
// Before this existed, handlers used bare r.BasicAuth() — a browser admin
// logged in via the Web UI was silently recorded as "anonymous" in every
// audit row and uploaded_by field.
func RequestActor(r *http.Request) string {
	if u := sessions.UserFromContext(r.Context()); u != "" {
		return u
	}
	if u, _, ok := r.BasicAuth(); ok {
		return u
	}
	return "anonymous"
}
