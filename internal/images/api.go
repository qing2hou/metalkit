package images

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// API surface (all under /api/v1/images):
//
//   POST   /uploads                   init upload session
//   GET    /uploads/{uid}             session status (chunks done, etc.)
//   PUT    /uploads/{uid}/chunks/{n}  upload chunk n (1-based, raw body)
//   POST   /uploads/{uid}/finalize    assemble + verify + commit
//   DELETE /uploads/{uid}             abort: remove session + chunks
//
//   GET    /                          list images
//   GET    /{id}                      one image
//   DELETE /{id}                      delete image (catalog row + file)
//
// All endpoints require Basic Auth (handled by httpd.basicAuth which matches
// the /api/v1/images prefix). The Web UI is the primary consumer; agents do
// not upload images.

// API binds the store + metadata extractor to HTTP handlers.
type API struct {
	store     *Store
	extractor metadataExtractor
	logger    *slog.Logger
}

// NewAPI constructs an API. extractor may be nil; in that case Finalize
// records images with fallback metadata.
func NewAPI(store *Store, extractor metadataExtractor, logger *slog.Logger) *API {
	return &API{store: store, extractor: extractor, logger: logger}
}

// RegisterRoutes mounts the images endpoints under /api/v1/images on mux.
func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/images/uploads", a.initUpload)
	mux.HandleFunc("GET /api/v1/images/uploads/{uid}", a.getUpload)
	mux.HandleFunc("PUT /api/v1/images/uploads/{uid}/chunks/{n}", a.putChunk)
	mux.HandleFunc("POST /api/v1/images/uploads/{uid}/finalize", a.finalize)
	mux.HandleFunc("DELETE /api/v1/images/uploads/{uid}", a.abortUpload)
	mux.HandleFunc("GET /api/v1/images", a.listImages)
	mux.HandleFunc("GET /api/v1/images/{id}", a.getImage)
	mux.HandleFunc("DELETE /api/v1/images/{id}", a.deleteImage)
	// Agent-facing blob download. Unguessable 32-hex ID acts as the capability
	// token; no Basic Auth (live-boot agents have no credential store, same as
	// /boot/* static files). http.ServeFile handles Range / If-Modified-Since.
	mux.HandleFunc("GET /api/v1/agent/images/{id}/blob", a.getBlob)
}

// initUpload body (JSON).
type initUploadReq struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	Family         string `json:"family"`
	Notes          string `json:"notes"`
	ExpectedSHA256 string `json:"expected_sha256"`
	TotalSize      int64  `json:"total_size"`
	ChunkSize      int64  `json:"chunk_size"`
}

func (a *API) initUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var in initUploadReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	// Best-effort backfill from filename when operator left family/version
	// blank. Explicit operator values always win (see MergeDetected).
	input := CreateUploadInput{
		Name:           in.Name,
		Version:        in.Version,
		Family:         in.Family,
		Notes:          in.Notes,
		ExpectedSHA256: strings.ToLower(strings.TrimSpace(in.ExpectedSHA256)),
		TotalSize:      in.TotalSize,
		ChunkSize:      in.ChunkSize,
		UploadedBy:     basicAuthUser(r),
	}
	det := DetectFromFilename(in.Name)
	// Reject obvious operator-vs-detection conflicts on family at the API
	// boundary, before any chunk hits disk. The classic footgun this stops:
	// uploading CentOS-7-*.qcow2 with family=rhel will silently produce a
	// "rhel" image that profile.os_family=rhel7 can't bind against, and the
	// failure only surfaces at install time. Family case-insensitive. We do
	// NOT enforce the same on version: version is freeform metadata operators
	// often customise (e.g. "centos7.9-our-build"); only family is
	// load-bearing for installer code-path selection.
	if det.Family != "" && strings.TrimSpace(in.Family) != "" &&
		!strings.EqualFold(strings.TrimSpace(in.Family), det.Family) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"family mismatch: filename %q looks like family=%q but you supplied family=%q. "+
				"Either leave family blank to use auto-detection, or rename the file to match.",
			in.Name, det.Family, in.Family))
		return
	}
	input = MergeDetected(input, det)
	sess, err := a.store.CreateUpload(r.Context(), input)
	if errors.Is(err, ErrDuplicate) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		// Any other error is a client-side validation failure (bad sha,
		// size out of range, …) — surface as 400.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (a *API) getUpload(w http.ResponseWriter, r *http.Request) {
	uid, ok := validImageID(w, r.PathValue("uid"))
	if !ok {
		return
	}
	sess, err := a.store.GetUpload(r.Context(), uid)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "upload session not found")
		return
	}
	if err != nil {
		a.logger.Error("get upload", "err", err, "uid", uid)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (a *API) putChunk(w http.ResponseWriter, r *http.Request) {
	uid, ok := validImageID(w, r.PathValue("uid"))
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "invalid chunk number")
		return
	}

	// Cap body size: max one chunk's worth. We don't know the session's
	// chunk_size without a DB round-trip; cap at MaxChunkSize+8 so even an
	// adversarial client can't stream forever.
	r.Body = http.MaxBytesReader(w, r.Body, MaxChunkSize+8)

	expectedSHA := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Chunk-Sha256")))

	written, err := a.store.WriteChunk(r.Context(), uid, n, r.Body, expectedSHA)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "upload session not found")
		return
	}
	if errors.As(err, &ErrSHAMismatch{}) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		// Validation errors (length mismatch, chunk out of range) surface as 400.
		if strings.Contains(err.Error(), "length") || strings.Contains(err.Error(), "out of range") || strings.Contains(err.Error(), "sha256") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("write chunk", "err", err, "uid", uid, "n", n)
		writeError(w, http.StatusInternalServerError, "chunk write failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chunk":         n,
		"bytes_written": written,
	})
}

func (a *API) finalize(w http.ResponseWriter, r *http.Request) {
	uid, ok := validImageID(w, r.PathValue("uid"))
	if !ok {
		return
	}
	res, err := a.store.FinalizeUpload(r.Context(), uid, a.extractor)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "upload session not found")
		return
	}
	if errors.Is(err, ErrDuplicate) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.As(err, &ErrSHAMismatch{}) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		// "missing" or size-mismatch is client error; everything else is 500.
		if strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), "size") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("finalize", "err", err, "uid", uid)
		writeError(w, http.StatusInternalServerError, "finalize failed")
		return
	}
	writeJSON(w, http.StatusCreated, res.Image)
}

func (a *API) abortUpload(w http.ResponseWriter, r *http.Request) {
	uid, ok := validImageID(w, r.PathValue("uid"))
	if !ok {
		return
	}
	if err := a.store.AbortUpload(r.Context(), uid); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "upload session not found")
			return
		}
		a.logger.Error("abort upload", "err", err, "uid", uid)
		writeError(w, http.StatusInternalServerError, "abort failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listImages(w http.ResponseWriter, r *http.Request) {
	imgs, err := a.store.ListImages(r.Context())
	if err != nil {
		a.logger.Error("list images", "err", err)
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, imgs)
}

func (a *API) getImage(w http.ResponseWriter, r *http.Request) {
	id, ok := validImageID(w, r.PathValue("id"))
	if !ok {
		return
	}
	img, err := a.store.GetImage(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	if err != nil {
		a.logger.Error("get image", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, img)
}

// getBlob streams the on-disk image file. Used by the live-boot agent during
// install; the unguessable 32-hex ID is the capability check and so the path
// is exempt from Basic Auth (matches the policy for /boot/* static files).
// http.ServeFile gives us Range support and 304s for free.
func (a *API) getBlob(w http.ResponseWriter, r *http.Request) {
	id, ok := validImageID(w, r.PathValue("id"))
	if !ok {
		return
	}
	img, err := a.store.GetImage(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	if err != nil {
		a.logger.Error("get blob: load image", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, a.store.FinalPath(img.SHA256, img.Format))
}

func (a *API) deleteImage(w http.ResponseWriter, r *http.Request) {
	id, ok := validImageID(w, r.PathValue("id"))
	if !ok {
		return
	}
	img, err := a.store.DeleteImageFile(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	if err != nil {
		a.logger.Error("delete image", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, img)
}

// validImageID enforces the 32-hex form produced by newImageID/newUploadID.
func validImageID(w http.ResponseWriter, raw string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if !imageIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid id")
		return "", false
	}
	return id, true
}

// basicAuthUser returns the Basic Auth username from the request, or "" if
// auth was not present. Used as the audit "uploaded_by" field.
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
