package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// maxReportBytes caps inbound /report bodies. 5 MiB is generous: a typical
// agent payload is under 100 KiB, but PCI/SMART blobs can balloon.
const maxReportBytes = 5 * 1024 * 1024

var (
	uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	macRE  = regexp.MustCompile(`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`)
)

// RegisterRoutes mounts the inventory HTTP API on mux under /api/v1/.
// Logging of the outer envelope is left to the host's middleware; this layer
// only logs per-handler decisions that are interesting in isolation.
func RegisterRoutes(mux *http.ServeMux, store *Store, logger *slog.Logger) {
	h := &apiHandlers{store: store, logger: logger}
	mux.HandleFunc("POST /api/v1/report", h.postReport)
	mux.HandleFunc("POST /api/v1/heartbeat/{uuid}", h.postHeartbeat)
	mux.HandleFunc("GET /api/v1/machines", h.listMachines)
	mux.HandleFunc("GET /api/v1/machines/{uuid}", h.getMachine)
	mux.HandleFunc("GET /api/v1/machines/{uuid}/reports", h.listReports)
	mux.HandleFunc("GET /api/v1/machines/{uuid}/reports/{id}", h.getReport)
	mux.HandleFunc("GET /api/v1/lookup", h.lookup)
}

// ActiveJobChecker reports whether a machine currently has a pending/running
// job. Returning (nil, nil-like-error) signals "no active job". A non-nil Job
// blocks deletion.
type ActiveJobChecker interface {
	HasActiveJob(ctx context.Context, machineUUID string) (bool, error)
}

// BindingDeleter wipes the bindings row for a machine UUID. Implementations
// should return nil (not an error) when the binding is already absent.
type BindingDeleter interface {
	DeleteBindingForMachine(ctx context.Context, machineUUID string) error
}

// RegisterDelete adds DELETE /api/v1/machines/{uuid}. It is a separate call so
// callers in tests can mount inventory routes without the cross-package wiring.
// Both deps are required: jobs guards the operation, bindings clears the
// non-cascading FK so the machine DELETE can land.
func RegisterDelete(mux *http.ServeMux, store *Store, jobs ActiveJobChecker, bindings BindingDeleter, logger *slog.Logger) {
	h := &apiHandlers{store: store, jobs: jobs, bindings: bindings, logger: logger}
	mux.HandleFunc("DELETE /api/v1/machines/{uuid}", h.deleteMachine)
}

type apiHandlers struct {
	store    *Store
	jobs     ActiveJobChecker
	bindings BindingDeleter
	logger   *slog.Logger
}

func (h *apiHandlers) postReport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxReportBytes)
	var rep Report
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if rep.SchemaVersion != SchemaVersion {
		writeError(w, http.StatusBadRequest, "unsupported schema_version")
		return
	}
	if strings.TrimSpace(rep.Machine.SMBIOSUUID) == "" {
		writeError(w, http.StatusBadRequest, "machine.smbios_uuid is required")
		return
	}

	uuid, reportID, err := h.store.UpsertReport(r.Context(), &rep)
	if err != nil {
		h.logger.Error("upsert report failed", "err", err)
		writeError(w, http.StatusInternalServerError, "upsert failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uuid":      uuid,
		"report_id": reportID,
	})
}

func (h *apiHandlers) postHeartbeat(w http.ResponseWriter, r *http.Request) {
	uuid, ok := validUUID(w, r.PathValue("uuid"))
	if !ok {
		return
	}
	err := h.store.Heartbeat(r.Context(), uuid)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "machine not found")
		return
	}
	if err != nil {
		h.logger.Error("heartbeat failed", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "heartbeat failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *apiHandlers) listMachines(w http.ResponseWriter, r *http.Request) {
	machines, err := h.store.ListMachines(r.Context())
	if err != nil {
		h.logger.Error("list machines failed", "err", err)
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, machines)
}

func (h *apiHandlers) getMachine(w http.ResponseWriter, r *http.Request) {
	uuid, ok := validUUID(w, r.PathValue("uuid"))
	if !ok {
		return
	}
	rep, err := h.store.LatestReport(r.Context(), uuid)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "machine not found")
		return
	}
	if err != nil {
		h.logger.Error("latest report failed", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (h *apiHandlers) listReports(w http.ResponseWriter, r *http.Request) {
	uuid, ok := validUUID(w, r.PathValue("uuid"))
	if !ok {
		return
	}
	// Distinguish "no machine" from "no reports yet" by checking existence first.
	if _, err := h.store.LatestReport(r.Context(), uuid); errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "machine not found")
		return
	} else if err != nil {
		h.logger.Error("machine probe failed", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	metas, err := h.store.ListReports(r.Context(), uuid)
	if err != nil {
		h.logger.Error("list reports failed", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "list failed")
		return
	}
	writeJSON(w, http.StatusOK, metas)
}

func (h *apiHandlers) getReport(w http.ResponseWriter, r *http.Request) {
	uuid, ok := validUUID(w, r.PathValue("uuid"))
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid report id")
		return
	}
	rep, err := h.store.GetReport(r.Context(), uuid, id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "report not found")
		return
	}
	if err != nil {
		h.logger.Error("get report failed", "err", err, "uuid", uuid, "id", id)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (h *apiHandlers) deleteMachine(w http.ResponseWriter, r *http.Request) {
	uuid, ok := validUUID(w, r.PathValue("uuid"))
	if !ok {
		return
	}
	// Refuse the delete when a pending/running job is in flight — the agent
	// would otherwise keep reporting to a job whose machine row no longer
	// exists. Operator's intent is unclear in that case; surface it.
	if h.jobs != nil {
		active, err := h.jobs.HasActiveJob(r.Context(), uuid)
		if err != nil {
			h.logger.Error("delete: active-job check failed", "err", err, "uuid", uuid)
			writeError(w, http.StatusInternalServerError, "active-job check failed")
			return
		}
		if active {
			writeError(w, http.StatusConflict, "machine has a pending/running job — cancel it first")
			return
		}
	}
	// Bindings FK references machines without ON DELETE CASCADE; wipe the
	// binding row explicitly. Absent binding is fine — surface only real errors.
	if h.bindings != nil {
		if err := h.bindings.DeleteBindingForMachine(r.Context(), uuid); err != nil {
			h.logger.Error("delete: binding wipe failed", "err", err, "uuid", uuid)
			writeError(w, http.StatusInternalServerError, "binding wipe failed")
			return
		}
	}
	if err := h.store.Delete(r.Context(), uuid); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "machine not found")
			return
		}
		h.logger.Error("delete machine failed", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	h.logger.Info("machine deleted", "uuid", uuid)
	w.WriteHeader(http.StatusNoContent)
}

func (h *apiHandlers) lookup(w http.ResponseWriter, r *http.Request) {
	mac := r.URL.Query().Get("mac")
	if !macRE.MatchString(mac) {
		writeError(w, http.StatusBadRequest, "mac must be 6 colon-separated hex octets")
		return
	}
	match, err := h.store.LookupByMAC(r.Context(), strings.ToLower(mac))
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "mac not found")
		return
	}
	if err != nil {
		h.logger.Error("mac lookup failed", "err", err, "mac", mac)
		writeError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, match)
}

// validUUID enforces the canonical lowercase-hex-dashed form. Anything else
// is rejected with 400 — the underlying SMBIOS UUID is always emitted in
// this shape by dmidecode, and accepting upper/lower mixes invites duplicate
// rows from agents that drift.
func validUUID(w http.ResponseWriter, raw string) (string, bool) {
	u := strings.ToLower(strings.TrimSpace(raw))
	if !uuidRE.MatchString(u) {
		writeError(w, http.StatusBadRequest, "invalid uuid")
		return "", false
	}
	return u, true
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
