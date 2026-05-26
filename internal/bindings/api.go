package bindings

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// API surface (all behind Basic Auth):
//
//   GET    /api/v1/bindings                       list
//   GET    /api/v1/bindings/{machine_uuid}        one
//   PUT    /api/v1/bindings/{machine_uuid}        upsert (creates or replaces)
//   DELETE /api/v1/bindings/{machine_uuid}        remove
//
// There is no POST: bindings are PK'd by machine_uuid which lives in the URL,
// so PUT is the only meaningful write verb. Re-PUTting the same machine_uuid
// overwrites the row in place (and only the row updated_at changes — historical
// (image, profile) tuples live in the jobs table).

// API binds the store to HTTP handlers.
type API struct {
	store  *Store
	logger *slog.Logger
}

// NewAPI constructs an API.
func NewAPI(store *Store, logger *slog.Logger) *API {
	return &API{store: store, logger: logger}
}

// RegisterRoutes mounts binding endpoints on mux.
func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/bindings", a.list)
	mux.HandleFunc("GET /api/v1/bindings/{uuid}", a.get)
	mux.HandleFunc("PUT /api/v1/bindings/{uuid}", a.upsert)
	mux.HandleFunc("DELETE /api/v1/bindings/{uuid}", a.delete)
	mux.HandleFunc("GET /api/v1/bindings/{uuid}/password", a.getPassword)
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	bs, err := a.store.List(r.Context())
	if err != nil {
		a.logger.Error("list bindings", "err", err)
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, bs)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	uuid := strings.ToLower(strings.TrimSpace(r.PathValue("uuid")))
	b, err := a.store.Get(r.Context(), uuid)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "binding not found")
		return
	}
	if err != nil {
		// validateMachineUUID failure surfaces as 400, everything else as 500.
		if strings.Contains(err.Error(), "machine_uuid") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("get binding", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (a *API) upsert(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var in UpsertInput
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in.MachineUUID = r.PathValue("uuid")
	in.UpdatedBy = basicAuthUser(r)

	b, err := a.store.Upsert(r.Context(), in)
	switch {
	case errors.Is(err, ErrMachineUnknown):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case errors.Is(err, ErrImageUnknown), errors.Is(err, ErrProfileUnknown):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (a *API) delete(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if err := a.store.Delete(r.Context(), uuid); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "binding not found")
			return
		}
		if strings.Contains(err.Error(), "machine_uuid") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("delete binding", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getPassword returns the stored plaintext root password. Behind Basic Auth
// like the rest of the API; the access is logged so an admin can audit who
// pulled which password. Returns 404 if the binding has no password set
// (the operator should rely on the profile default in that case).
func (a *API) getPassword(w http.ResponseWriter, r *http.Request) {
	uuid := strings.ToLower(strings.TrimSpace(r.PathValue("uuid")))
	pt, err := a.store.GetPassword(r.Context(), uuid)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "no password set for this binding")
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "machine_uuid") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("get binding password", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	a.logger.Info("binding password viewed",
		"machine_uuid", uuid, "actor", basicAuthUser(r))
	writeJSON(w, http.StatusOK, map[string]string{"password": pt})
}

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
