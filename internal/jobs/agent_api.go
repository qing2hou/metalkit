package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"metalkit/internal/bindings"
	"metalkit/internal/images"
	"metalkit/internal/profiles"
	"metalkit/internal/util"
)

// BindingFetcher is the slice of bindings.Store the agent spec endpoint needs.
// Kept tiny so tests can inject fakes without dragging in the full store.
type BindingFetcher interface {
	Get(ctx context.Context, machineUUID string) (*bindings.Binding, error)
	// GetPassword returns the plaintext root password for the binding, or
	// bindings.ErrNotFound when no per-binding password is set (caller falls
	// back to the profile default).
	GetPassword(ctx context.Context, machineUUID string) (string, error)
}

// ProfileFetcher is the slice of profiles.Store the agent spec endpoint needs.
type ProfileFetcher interface {
	Get(ctx context.Context, id string) (*profiles.Profile, error)
}

// ImageFetcher is the slice of images.Store the agent spec endpoint needs.
type ImageFetcher interface {
	GetImage(ctx context.Context, id string) (*images.Image, error)
}

// AgentAPI exposes the agent-facing slice of the jobs state machine:
//
//   GET  /api/v1/agent/jobs/current?machine_uuid=<uuid>   poll for assigned job
//   GET  /api/v1/agent/jobs/{id}/spec?machine_uuid=<uuid> fetch full InstallSpec
//   POST /api/v1/agent/jobs/{id}/claim                    pending → running
//   POST /api/v1/agent/jobs/{id}/stage                    update stage marker
//   POST /api/v1/agent/jobs/{id}/logs                     append log line
//   POST /api/v1/agent/jobs/{id}/succeed                  running → succeeded
//   POST /api/v1/agent/jobs/{id}/fail                     {pending|running} → failed
//
// Auth model: none. Live-boot agents have no credential store, so these paths
// are exempt from Basic Auth (same as /api/v1/report and /api/v1/heartbeat/*).
// Every state-changing endpoint requires a machine_uuid in the body that must
// match the job's machine_uuid — this is a consistency check that prevents
// agents from accidentally driving jobs that belong to a different machine,
// NOT a security boundary. Real auth (token-based) is planned for M2.5.
type AgentAPI struct {
	store    *Store
	bindings BindingFetcher
	profiles ProfileFetcher
	images   ImageFetcher
	logger   *slog.Logger
}

// NewAgentAPI constructs an AgentAPI without spec-handler fetchers — the spec
// endpoint will 503 in this mode. Kept for back-compat with tests that only
// exercise the state-machine endpoints.
func NewAgentAPI(store *Store, logger *slog.Logger) *AgentAPI {
	return &AgentAPI{store: store, logger: logger}
}

// NewAgentAPIWithFetchers is the production constructor: wires in the
// bindings/profiles/images stores so the spec endpoint can assemble an
// InstallSpec. bindings.Store, profiles.Store and images.Store all satisfy
// the corresponding fetcher interfaces directly.
func NewAgentAPIWithFetchers(store *Store, bindings BindingFetcher, profiles ProfileFetcher, images ImageFetcher, logger *slog.Logger) *AgentAPI {
	return &AgentAPI{
		store:    store,
		bindings: bindings,
		profiles: profiles,
		images:   images,
		logger:   logger,
	}
}

// RegisterRoutes mounts the agent endpoints on mux.
func (a *AgentAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agent/jobs/current", a.current)
	mux.HandleFunc("GET /api/v1/agent/jobs/{id}/spec", a.getSpec)
	mux.HandleFunc("POST /api/v1/agent/jobs/{id}/claim", a.claim)
	mux.HandleFunc("POST /api/v1/agent/jobs/{id}/stage", a.stage)
	mux.HandleFunc("POST /api/v1/agent/jobs/{id}/logs", a.appendLog)
	mux.HandleFunc("POST /api/v1/agent/jobs/{id}/succeed", a.succeed)
	mux.HandleFunc("POST /api/v1/agent/jobs/{id}/fail", a.fail)
}

// agentBody is the common envelope for state-changing endpoints. Every call
// must include machine_uuid so the controller can verify the job belongs to
// the caller. Specific endpoints (stage, logs, fail) extend it.
type agentBody struct {
	MachineUUID string `json:"machine_uuid"`
	Stage       string `json:"stage,omitempty"`
	Level       string `json:"level,omitempty"`
	Message     string `json:"message,omitempty"`
	Error       string `json:"error,omitempty"`
}

func (a *AgentAPI) current(w http.ResponseWriter, r *http.Request) {
	muuid := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("machine_uuid")))
	if muuid == "" {
		writeError(w, http.StatusBadRequest, "machine_uuid query param required")
		return
	}
	j, err := a.store.CurrentForMachine(r.Context(), muuid)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "no in-flight job")
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "machine_uuid") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("agent current", "err", err, "machine_uuid", muuid)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// getSpec returns the complete InstallSpec for a job — binding + profile +
// image catalog metadata + blob URL. Like the other agent endpoints, no auth,
// just a machine_uuid consistency check (query string here, not body).
func (a *AgentAPI) getSpec(w http.ResponseWriter, r *http.Request) {
	if a.bindings == nil || a.profiles == nil || a.images == nil {
		writeError(w, http.StatusServiceUnavailable, "spec endpoint not configured")
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	muuid := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("machine_uuid")))
	job, ok := a.verifyOwnership(w, r, id, muuid)
	if !ok {
		return
	}

	binding, err := a.bindings.Get(r.Context(), job.MachineUUID)
	if err != nil {
		// A job exists for this machine, so a binding must exist too — anything
		// here (including ErrNotFound) signals corrupt state, not a client bug.
		a.logger.Error("agent spec: load binding", "err", err, "job_id", id, "machine_uuid", job.MachineUUID)
		writeError(w, http.StatusInternalServerError, "load binding failed")
		return
	}
	profile, err := a.profiles.Get(r.Context(), job.ProfileID)
	if err != nil {
		a.logger.Error("agent spec: load profile", "err", err, "job_id", id, "profile_id", job.ProfileID)
		writeError(w, http.StatusInternalServerError, "load profile failed")
		return
	}
	image, err := a.images.GetImage(r.Context(), job.ImageID)
	if err != nil {
		a.logger.Error("agent spec: load image", "err", err, "job_id", id, "image_id", job.ImageID)
		writeError(w, http.StatusInternalServerError, "load image failed")
		return
	}

	// Per-binding target_disk override: if the operator picked a specific disk
	// on the binding (one-shot override from the install modal), substitute it
	// into the profile copy before sending the spec. The agent / installer
	// contract still reads target_disk off the profile, so the override is
	// transparent downstream.
	if binding.TargetDisk != nil {
		profile.TargetDisk = *binding.TargetDisk
	}

	// Per-binding bond override: same one-shot semantics as target_disk.
	// When set, also force nic_selector=auto since validateBond's invariant
	// (bond.Slaves selects the physical NICs by MAC, single-NIC selector is
	// irrelevant) must still hold downstream.
	if binding.Bond != nil {
		bondCopy := *binding.Bond
		profile.Network.Bond = &bondCopy
		profile.Network.NICSelector = "auto"
	}

	// Per-binding password override: if the operator set a plaintext password
	// on the binding, encrypt-hash it now and overwrite the profile's default
	// hash before sending the spec downstream. Keeps the agent / installer
	// contract unchanged (still receives a single $6$… hash on the profile).
	if binding.HasPassword {
		plain, err := a.bindings.GetPassword(r.Context(), job.MachineUUID)
		if err != nil {
			a.logger.Error("agent spec: load binding password", "err", err,
				"job_id", id, "machine_uuid", job.MachineUUID)
			writeError(w, http.StatusInternalServerError, "load binding password failed")
			return
		}
		hash, err := util.CryptSHA512(r.Context(), plain)
		if err != nil {
			a.logger.Error("agent spec: crypt binding password", "err", err,
				"job_id", id, "machine_uuid", job.MachineUUID)
			writeError(w, http.StatusInternalServerError, "crypt binding password failed")
			return
		}
		profile.RootPasswordHash = hash
	}

	spec := InstallSpec{
		JobID:        job.ID,
		MachineUUID:  job.MachineUUID,
		ImageID:      image.ID,
		ImageBlobURL: "/api/v1/agent/images/" + image.ID + "/blob",
		ImageSHA256:  image.SHA256,
		ImageFormat:  image.Format,
		Profile:      *profile,
		Binding:      *binding,
	}
	writeJSON(w, http.StatusOK, spec)
}

// verifyOwnership loads the job and confirms its machine_uuid matches the
// caller's claimed machine_uuid. Returns (job, true) on match, (nil, false)
// otherwise — caller writes the appropriate error response.
func (a *AgentAPI) verifyOwnership(w http.ResponseWriter, r *http.Request, jobID, muuid string) (*Job, bool) {
	if muuid == "" {
		writeError(w, http.StatusBadRequest, "machine_uuid is required")
		return nil, false
	}
	j, err := a.store.Get(r.Context(), jobID)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return nil, false
	}
	if err != nil {
		if strings.Contains(err.Error(), "job_id") {
			writeError(w, http.StatusBadRequest, err.Error())
			return nil, false
		}
		a.logger.Error("agent verify ownership", "err", err, "job_id", jobID)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return nil, false
	}
	if !strings.EqualFold(j.MachineUUID, muuid) {
		writeError(w, http.StatusForbidden, "machine_uuid does not match job owner")
		return nil, false
	}
	return j, true
}

func (a *AgentAPI) decode(w http.ResponseWriter, r *http.Request) (agentBody, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var b agentBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return agentBody{}, false
	}
	b.MachineUUID = strings.ToLower(strings.TrimSpace(b.MachineUUID))
	return b, true
}

func (a *AgentAPI) claim(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	body, ok := a.decode(w, r)
	if !ok {
		return
	}
	if _, ok := a.verifyOwnership(w, r, id, body.MachineUUID); !ok {
		return
	}
	j, err := a.store.Claim(r.Context(), id)
	if errors.Is(err, ErrInvalidTransition) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		a.logger.Error("agent claim", "err", err, "job_id", id)
		writeError(w, http.StatusInternalServerError, "claim failed")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (a *AgentAPI) stage(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	body, ok := a.decode(w, r)
	if !ok {
		return
	}
	if _, ok := a.verifyOwnership(w, r, id, body.MachineUUID); !ok {
		return
	}
	err := a.store.UpdateStage(r.Context(), id, body.Stage)
	if errors.Is(err, ErrInvalidTransition) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "stage") || strings.Contains(err.Error(), "job_id") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("agent stage", "err", err, "job_id", id)
		writeError(w, http.StatusInternalServerError, "stage update failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *AgentAPI) appendLog(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	body, ok := a.decode(w, r)
	if !ok {
		return
	}
	if _, ok := a.verifyOwnership(w, r, id, body.MachineUUID); !ok {
		return
	}
	err := a.store.AppendLog(r.Context(), id, body.Level, body.Message)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "level") || strings.Contains(err.Error(), "job_id") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.logger.Error("agent append log", "err", err, "job_id", id)
		writeError(w, http.StatusInternalServerError, "log failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *AgentAPI) succeed(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	body, ok := a.decode(w, r)
	if !ok {
		return
	}
	if _, ok := a.verifyOwnership(w, r, id, body.MachineUUID); !ok {
		return
	}
	err := a.store.Succeed(r.Context(), id)
	if errors.Is(err, ErrInvalidTransition) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		a.logger.Error("agent succeed", "err", err, "job_id", id)
		writeError(w, http.StatusInternalServerError, "succeed failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *AgentAPI) fail(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	body, ok := a.decode(w, r)
	if !ok {
		return
	}
	if _, ok := a.verifyOwnership(w, r, id, body.MachineUUID); !ok {
		return
	}
	err := a.store.Fail(r.Context(), id, body.Error)
	if errors.Is(err, ErrInvalidTransition) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		a.logger.Error("agent fail", "err", err, "job_id", id)
		writeError(w, http.StatusInternalServerError, "fail failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
