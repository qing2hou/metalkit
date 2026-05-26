package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// API surface (operator side, behind Basic Auth):
//
//   GET    /api/v1/jobs                       list (?machine_uuid= ?status= ?limit=)
//   GET    /api/v1/jobs/{id}                  one
//   GET    /api/v1/jobs/{id}/logs             logs (?since_id= ?limit=)
//   POST   /api/v1/jobs/{id}/cancel           cancel a pending/running job
//
// Agent-facing endpoints (claim, append log, update stage, succeed/fail) live
// under /api/v1/agent/jobs/* and are mounted by a separate handler (M2.3-6) so
// they can use a token-based auth model distinct from the operator's Basic Auth.

// API binds the jobs store to HTTP handlers.
type API struct {
	store  *Store
	logger *slog.Logger
}

// NewAPI constructs an API.
func NewAPI(store *Store, logger *slog.Logger) *API {
	return &API{store: store, logger: logger}
}

// RegisterRoutes mounts operator endpoints on mux.
func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/jobs", a.list)
	mux.HandleFunc("GET /api/v1/jobs/{id}", a.get)
	mux.HandleFunc("GET /api/v1/jobs/{id}/logs", a.logs)
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", a.cancel)
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{
		MachineUUID: strings.ToLower(strings.TrimSpace(q.Get("machine_uuid"))),
		Status:      strings.TrimSpace(q.Get("status")),
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		f.Limit = n
	}
	js, err := a.store.List(r.Context(), f)
	if err != nil {
		if strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "machine_uuid") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("list jobs", "err", err)
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, js)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	j, err := a.store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "job_id") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("get job", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	q := r.URL.Query()
	var sinceID int64
	if s := q.Get("since_id"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid since_id")
			return
		}
		sinceID = n
	}
	limit := 200
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	logs, err := a.store.Logs(r.Context(), id, sinceID, limit)
	if err != nil {
		if strings.Contains(err.Error(), "job_id") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("get logs", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "logs failed")
		return
	}
	writeJSON(w, http.StatusOK, logs)
}

func (a *API) cancel(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	err := a.store.Cancel(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if errors.Is(err, ErrInvalidTransition) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "job_id") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("cancel job", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "cancel failed")
		return
	}
	// Record who cancelled so operators can trace it in the logs.
	_ = a.store.AppendLog(r.Context(), id, "info", fmt.Sprintf("cancelled by %s", basicAuthUser(r)))
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers ----

func basicAuthUser(r *http.Request) string {
	if u, _, ok := r.BasicAuth(); ok {
		return u
	}
	return "anonymous"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
