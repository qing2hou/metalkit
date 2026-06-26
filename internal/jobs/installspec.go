package jobs

import (
	"metalkit/internal/bindings"
	"metalkit/internal/profiles"
)

// InstallSpec is the complete blueprint the agent needs to install one machine.
// It bundles the binding (per-machine assignment), the profile (template) and
// just enough image catalog metadata for the agent to download + verify the
// blob.  The agent fetches it once at the start of a job via:
//
//	GET /api/v1/agent/jobs/{id}/spec?machine_uuid=<uuid>
//
// One round trip, one consistent view — cheaper than wiring three separate
// agent endpoints and easier to audit ("agent X took blueprint Y at time T").
//
// Notes on what's *not* sanitized:
//
//   - Profile carries RootPasswordHash (an /etc/shadow-ready sha512crypt
//     string). The installer must write it into cloud-init user-data, so it
//     has to leave the controller. The agent transport is plain HTTP on the
//     PXE network; in a production deployment the PXE network is expected to
//     be management-only — see plan §B4.
//   - BMC credentials are *not* here. Those stay on the controller and are
//     only used by the orchestrator (jobs.Orchestrator) to drive ipmitool.
//
// Field stability: the spec is consumed by the live-boot agent that ships in
// the same release as the controller, so we don't need cross-version stability
// inside M2. If the schema diverges, bump a version literal and key off it.
type InstallSpec struct {
	JobID        string           `json:"job_id"`
	MachineUUID  string           `json:"machine_uuid"`
	ImageID      string           `json:"image_id"`
	ImageBlobURL string           `json:"image_blob_url"` // relative path; agent joins onto its baseURL
	ImageSHA256  string           `json:"image_sha256"`
	ImageFormat  string           `json:"image_format"` // "qcow2" or "raw"
	Profile      profiles.Profile `json:"profile"`
	Binding      bindings.Binding `json:"binding"`
	// NetworkRenderer selects which network config rendering strategy to use.
	// Empty string = "auto" (determined by OS detection at install time).
	NetworkRenderer string `json:"network_renderer,omitempty"`
	// Bootloader selects which bootloader installation strategy to use.
	// Empty string = "auto" (determined by OS detection at install time).
	Bootloader string `json:"bootloader,omitempty"`
}
