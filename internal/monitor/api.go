// api.go mounts the monitor HTTP surface:
//
//	POST /api/v1/agent/metrics          — monitor agents push samples here.
//	                                     Open (no auth) like /api/v1/report:
//	                                     implanted monitors have no credential
//	                                     store; the body carries the UUID.
//	GET  /api/v1/metrics                — latest sample per machine (UI).
//	GET  /api/v1/metrics/{uuid}         — recent history for one machine.
//
// The GET side is operator-facing and sits behind normal session/Basic
// auth (httpd.needsAuth covers /api/v1/metrics*). Only the agent POST
// path is open — matching the report/heartbeat precedent.
package monitor

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxPayloadBytes caps inbound metric bodies. A full sample is a few KB
// with hundreds of disks/interfaces; 1 MiB is ample headroom.
const maxPayloadBytes = 1 << 20

// latestWithin bounds how fresh a sample must be for /api/v1/metrics to
// include the machine. 3× the default 60s monitor interval absorbs one
// missed beat without flapping the UI.
const latestWithin = 3 * time.Minute

// API binds the store to HTTP handlers.
type API struct {
	store  *Store
	logger *slog.Logger
}

// NewAPI constructs an API. store must come from NewStore (already has
// schema applied).
func NewAPI(store *Store, logger *slog.Logger) *API {
	if logger == nil {
		logger = slog.Default()
	}
	return &API{store: store, logger: logger}
}

// RegisterRoutes mounts both the agent push endpoint and the operator
// query endpoints on mux.
func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/agent/metrics", a.push)
	mux.HandleFunc("GET /api/v1/metrics", a.latest)
	mux.HandleFunc("GET /api/v1/metrics/{uuid}", a.history)
}

// defaultHistoryLimit is the sample count returned when the caller doesn't
// ask. 60 samples ≈ 1h at the default 60s interval.
const defaultHistoryLimit = 60

func (a *API) push(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPayloadBytes)
	var p Payload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	p.MachineUUID = strings.ToLower(strings.TrimSpace(p.MachineUUID))
	if p.MachineUUID == "" {
		writeError(w, http.StatusBadRequest, "machine_uuid is required")
		return
	}
	if p.SchemaVersion != SchemaVersion {
		writeError(w, http.StatusBadRequest, "unsupported schema_version")
		return
	}
	// MonitoredAt missing/zero → server stamps now (Append falls back).
	if err := a.store.Append(r.Context(), p.MachineUUID, p); err != nil {
		a.logger.Error("monitor: append failed", "err", err, "machine_uuid", p.MachineUUID)
		writeError(w, http.StatusInternalServerError, "append failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Sample is one machine's latest metrics plus the freshness flag the UI
// needs to render a monitoring badge.
type Sample struct {
	MachineUUID string  `json:"machine_uuid"`
	AgentOnline bool    `json:"agent_online"`
	Payload     Payload `json:"payload"`
}

func (a *API) latest(w http.ResponseWriter, r *http.Request) {
	byUUID, err := a.store.Latest(r.Context(), latestWithin)
	if err != nil {
		a.logger.Error("monitor: latest failed", "err", err)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	out := make([]Sample, 0, len(byUUID))
	for uuid, p := range byUUID {
		out = append(out, Sample{MachineUUID: uuid, AgentOnline: true, Payload: p})
	}
	// Sort by UUID for stable UI ordering; latest() is a map.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].MachineUUID > out[j].MachineUUID; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) history(w http.ResponseWriter, r *http.Request) {
	uuid := strings.ToLower(strings.TrimSpace(r.PathValue("uuid")))
	if uuid == "" {
		writeError(w, http.StatusBadRequest, "uuid is required")
		return
	}
	limit := defaultHistoryLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "limit must be 1..1000")
			return
		}
		limit = n
	}
	samples, err := a.store.History(r.Context(), uuid, limit)
	if err != nil {
		a.logger.Error("monitor: history failed", "err", err, "uuid", uuid)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, samples)
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
