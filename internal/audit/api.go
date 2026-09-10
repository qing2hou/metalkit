// Package audit API: read-only /api/v1/audit endpoint for the operator UI.
package audit

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"metalkit/internal/sessions"
)

// API serves GET /api/v1/audit (list events). Requires operator auth —
// enforced by the httpd middleware; the handler itself only maps errors.
type API struct {
	store  *Store
	logger *slog.Logger
}

func NewAPI(store *Store, logger *slog.Logger) *API {
	return &API{store: store, logger: logger}
}

// Store returns the underlying store so wiring code (httpd) can reuse the
// same instance for middleware-level auditing.
func (a *API) Store() *Store {
	return a.store
}

func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/audit", a.list)
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	actor := sessions.UserFromContext(r.Context())

	opts := ListOptions{
		Actor:  r.URL.Query().Get("actor"),
		Action: r.URL.Query().Get("action"),
	}
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			opts.Limit = n
		}
	}
	if s := r.URL.Query().Get("offset"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			opts.Offset = n
		}
	}

	events, err := a.store.List(r.Context(), opts)
	if err != nil {
		a.logger.Error("audit list", "err", err, "actor", actor)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list failed"})
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
