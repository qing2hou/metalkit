// Package audit API: read-only /api/v1/audit endpoint for the operator UI.
package audit

import (
	"encoding/json"
	"errors"
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
	mux.HandleFunc("DELETE /api/v1/audit/{id}", a.remove)
	mux.HandleFunc("POST /api/v1/audit/purge", a.purge)
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	// The audit trail itself is admin-only: it records who performed every
	// mutation, so letting operators read it is fine in principle, but the
	// admin page also surfaces password_view events; keep it admin.
	if !sessions.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "reading the audit trail requires the admin role"})
		return
	}
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

// remove deletes one audit event. Admin-only: the audit trail is the
// record of who did what — trimming it is an admin act, and the deletion
// itself is audited.
func (a *API) remove(w http.ResponseWriter, r *http.Request) {
	if !sessions.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "deleting audit events requires the admin role"})
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid event id"})
		return
	}
	if err := a.store.DeleteEvent(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "event not found"})
			return
		}
		a.logger.Error("audit delete", "err", err, "id", id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delete failed"})
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// purge deletes every audit event; returns the count removed. Admin-only.
func (a *API) purge(w http.ResponseWriter, r *http.Request) {
	if !sessions.IsAdmin(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "purging the audit trail requires the admin role"})
		return
	}
	n, err := a.store.DeleteAll(r.Context())
	if err != nil {
		a.logger.Error("audit purge", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "purge failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": n})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
