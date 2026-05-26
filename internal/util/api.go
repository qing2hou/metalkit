package util

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

type API struct {
	logger *slog.Logger
}

func NewAPI(logger *slog.Logger) *API { return &API{logger: logger} }

func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/util/crypt-sha512", a.cryptSHA512)
}

type cryptRequest struct {
	Password string `json:"password"`
}
type cryptResponse struct {
	Hash string `json:"hash"`
}

func (a *API) cryptSHA512(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var in cryptRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	hash, err := CryptSHA512(r.Context(), in.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Don't log the password OR the hash.
	a.logger.Info("generated sha512crypt hash")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(cryptResponse{Hash: hash})
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
