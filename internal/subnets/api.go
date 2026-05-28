package subnets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// API surface (all under /api/v1/subnets, full subtree behind Basic Auth):
//
//   GET    /                  list subnets
//   POST   /                  create
//   GET    /{id}              get one
//   PUT    /{id}              partial update (only specified fields change)
//   DELETE /{id}              delete (bindings may reference subnets; deletion
//                             will be refused at the binding layer once that
//                             ref lands — for now subnet rows are independent)

// BindingRefCounter reports how many bindings reference a given subnet. Wired
// from cmd/controller so the subnets API can refuse to drop a row that is
// still pointed at by a binding (avoids dangling references — the bindings
// table does not yet have a real FK with ON DELETE RESTRICT).
type BindingRefCounter func(ctx context.Context, subnetID string) (int, error)

type API struct {
	store        *Store
	logger       *slog.Logger
	refCountFunc BindingRefCounter
}

func NewAPI(store *Store, logger *slog.Logger) *API {
	return &API{store: store, logger: logger}
}

// WithBindingRefCount registers a ref-count probe; DELETE will refuse with 422
// when the count > 0. Returns the API for chaining at wire-up time. nil disables
// the check (default — keeps tests simple).
func (a *API) WithBindingRefCount(fn BindingRefCounter) *API {
	a.refCountFunc = fn
	return a
}

func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/subnets", a.list)
	mux.HandleFunc("POST /api/v1/subnets", a.create)
	mux.HandleFunc("GET /api/v1/subnets/{id}", a.get)
	mux.HandleFunc("PUT /api/v1/subnets/{id}", a.update)
	mux.HandleFunc("DELETE /api/v1/subnets/{id}", a.delete)
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	subs, err := a.store.List(r.Context())
	if err != nil {
		a.logger.Error("list subnets", "err", err)
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, subs)
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
	sn, err := a.store.Create(r.Context(), in)
	if errors.Is(err, ErrDuplicateName) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sn)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	id, ok := validSubnetID(w, r.PathValue("id"))
	if !ok {
		return
	}
	sn, err := a.store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "subnet not found")
		return
	}
	if err != nil {
		a.logger.Error("get subnet", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, sn)
}

func (a *API) update(w http.ResponseWriter, r *http.Request) {
	id, ok := validSubnetID(w, r.PathValue("id"))
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
	in.UpdatedBy = basicAuthUser(r)
	sn, err := a.store.Update(r.Context(), id, in)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "subnet not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sn)
}

func (a *API) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := validSubnetID(w, r.PathValue("id"))
	if !ok {
		return
	}
	if a.refCountFunc != nil {
		n, err := a.refCountFunc(r.Context(), id)
		if err != nil {
			a.logger.Error("subnet refcount", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "refcount failed")
			return
		}
		if n > 0 {
			writeError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("subnet still in use by %d binding(s)", n))
			return
		}
	}
	if err := a.store.Delete(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "subnet not found")
			return
		}
		a.logger.Error("delete subnet", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validSubnetID(w http.ResponseWriter, raw string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if !subnetIDRE.MatchString(id) {
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
