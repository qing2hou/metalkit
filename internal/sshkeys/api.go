package sshkeys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"metalkit/internal/apiutil"
)

// API surface (all under /api/v1/sshkeys, behind Basic Auth):
//
//	GET    /                  list keys
//	POST   /                  create
//	GET    /{id}              get one
//	PUT    /{id}              partial update (only specified fields change)
//	DELETE /{id}              delete (refused with 422 while any binding
//	                          still references the key)

// BindingRefCounter reports how many bindings reference a given key. Wired
// from cmd/controller so the delete handler can refuse to drop a key that
// is still pointed at by a binding — same gate as subnets/profiles/images.
type BindingRefCounter func(ctx context.Context, keyID string) (int, error)

// BindingWriteLock is the bindings store's write lock (see
// bindings.Store.AcquireWriteLock). The delete handler holds it across
// ref-count + delete so a concurrent binding Upsert cannot squeeze its
// "key exists" check between the count and the DELETE — that race leaves a
// dangling ssh_key_id that 500s every later spec fetch for the machine.
type BindingWriteLock func() (release func())

type API struct {
	store        *Store
	logger       *slog.Logger
	refCountFunc BindingRefCounter
	writeLock    BindingWriteLock
}

func NewAPI(store *Store, logger *slog.Logger) *API {
	return &API{store: store, logger: logger}
}

// WithBindingRefCount registers a ref-count probe; DELETE will refuse with
// 422 when the count > 0. nil disables the check (default — keeps tests
// simple).
func (a *API) WithBindingRefCount(fn BindingRefCounter) *API {
	a.refCountFunc = fn
	return a
}

// WithBindingWriteLock registers the bindings write lock the delete handler
// holds across ref-count + delete. Only meaningful together with
// WithBindingRefCount; nil (default) keeps tests simple.
func (a *API) WithBindingWriteLock(fn BindingWriteLock) *API {
	a.writeLock = fn
	return a
}

func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/sshkeys", a.list)
	mux.HandleFunc("POST /api/v1/sshkeys", a.create)
	mux.HandleFunc("GET /api/v1/sshkeys/{id}", a.get)
	mux.HandleFunc("PUT /api/v1/sshkeys/{id}", a.update)
	mux.HandleFunc("DELETE /api/v1/sshkeys/{id}", a.delete)
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	keys, err := a.store.List(r.Context())
	if err != nil {
		a.logger.Error("list ssh keys", "err", err)
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, keys)
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
	in.CreatedBy = apiutil.RequestActor(r)
	k, err := a.store.Create(r.Context(), in)
	if errors.Is(err, ErrDuplicateName) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, k)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	id, ok := validKeyID(w, r.PathValue("id"))
	if !ok {
		return
	}
	k, err := a.store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "ssh key not found")
		return
	}
	if err != nil {
		a.logger.Error("get ssh key", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, k)
}

func (a *API) update(w http.ResponseWriter, r *http.Request) {
	id, ok := validKeyID(w, r.PathValue("id"))
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
	in.UpdatedBy = apiutil.RequestActor(r)
	k, err := a.store.Update(r.Context(), id, in)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "ssh key not found")
		return
	}
	if errors.Is(err, ErrDuplicateName) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, k)
}

func (a *API) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := validKeyID(w, r.PathValue("id"))
	if !ok {
		return
	}
	// Ref-count + delete under the bindings write lock (when wired): see
	// BindingWriteLock. The lock is only needed when the ref gate is on.
	if a.writeLock != nil && a.refCountFunc != nil {
		release := a.writeLock()
		defer release()
	}
	if a.refCountFunc != nil {
		n, err := a.refCountFunc(r.Context(), id)
		if err != nil {
			a.logger.Error("ssh key refcount", "err", err, "id", id)
			writeError(w, http.StatusInternalServerError, "refcount failed")
			return
		}
		if n > 0 {
			writeError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("ssh key still in use by %d binding(s)", n))
			return
		}
	}
	if err := a.store.Delete(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "ssh key not found")
			return
		}
		a.logger.Error("delete ssh key", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validKeyID(w http.ResponseWriter, raw string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if !sshKeyIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid id")
		return "", false
	}
	return id, true
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
