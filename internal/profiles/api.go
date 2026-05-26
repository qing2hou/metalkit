package profiles

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// API surface (all under /api/v1/profiles, full subtree behind Basic Auth):
//
//   GET    /                  list profiles
//   POST   /                  create
//   GET    /{id}              get one
//   PUT    /{id}              partial update (only specified fields change)
//   DELETE /{id}              delete (will be ON DELETE RESTRICT once bindings land)

// API binds the store to HTTP handlers.
type API struct {
	store  *Store
	logger *slog.Logger
}

// NewAPI constructs an API.
func NewAPI(store *Store, logger *slog.Logger) *API {
	return &API{store: store, logger: logger}
}

// RegisterRoutes mounts profile endpoints on mux.
func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/profiles", a.list)
	mux.HandleFunc("POST /api/v1/profiles", a.create)
	mux.HandleFunc("GET /api/v1/profiles/{id}", a.get)
	mux.HandleFunc("PUT /api/v1/profiles/{id}", a.update)
	mux.HandleFunc("DELETE /api/v1/profiles/{id}", a.delete)
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	ps, err := a.store.List(r.Context())
	if err != nil {
		a.logger.Error("list profiles", "err", err)
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var in CreateInput
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in.CreatedBy = basicAuthUser(r)
	p, err := a.store.Create(r.Context(), in)
	if errors.Is(err, ErrDuplicateName) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		// Treat all other errors as validation failures from the store layer.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	id, ok := validProfileID(w, r.PathValue("id"))
	if !ok {
		return
	}
	p, err := a.store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "profile not found")
		return
	}
	if err != nil {
		a.logger.Error("get profile", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) update(w http.ResponseWriter, r *http.Request) {
	id, ok := validProfileID(w, r.PathValue("id"))
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var in UpdateInput
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	p, err := a.store.Update(r.Context(), id, in)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "profile not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := validProfileID(w, r.PathValue("id"))
	if !ok {
		return
	}
	if err := a.store.Delete(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "profile not found")
			return
		}
		a.logger.Error("delete profile", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validProfileID(w http.ResponseWriter, raw string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if !profileIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid id")
		return "", false
	}
	return id, true
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
