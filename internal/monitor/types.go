// Package monitor is the metrics contract shared by the monitor component
// (cmd/monitor, implanted into installed systems by the installer) and the
// controller-side ingestion endpoint (internal/monitor/api.go).
//
// Design intent: this agent is deliberately NOT the install agent. The
// install agent (cmd/agent) lives only inside the Debian live image — it
// polls for jobs, writes disks, and dies with the live boot. The monitor
// agent is a separate, single-purpose component that gets copied into the
// installed OS when the profile/binding asks for it; all it does is collect
// lightweight metrics and POST them. Keeping the two apart means a monitor
// failure can never break an install, and the installed system carries no
// installer code.
//
// Metrics are small fixed-size samples — a full payload is a few hundred
// bytes — so a 60s cadence costs nothing on the management network.
package monitor

import "time"

// Payload is the POST body of /api/v1/agent/metrics.
type Payload struct {
	// MachineUUID is the SMBIOS UUID the controller already knows the
	// machine by (same identifier the install agent reports). Collected
	// from dmidecode output by the monitor.
	MachineUUID string `json:"machine_uuid"`
	// SchemaVersion lets the controller reject incompatible shapes.
	SchemaVersion int `json:"schema_version"`
	// MonitoredAt is when the sample was taken (not when it was sent).
	MonitoredAt time.Time `json:"monitored_at"`
	Metrics     Metrics   `json:"metrics"`
}

// SchemaVersion of the metrics payload.
const SchemaVersion = 1

// Metrics is one point-in-time sample of the host. Only fields every
// Linux distro exposes under /proc and /sys are collected — no external
// tools, no distro-specific paths. Counters (CPU jiffies, net bytes) are
// cumulative raw values; rates are computed by the consumer from deltas,
// keeping the payload format honest and stateless.
type Metrics struct {
	// CPU.
	CPUTotalJiffies uint64  `json:"cpu_total_jiffies"` // sum of all /proc/stat cpu lines
	CPUIdleJiffies  uint64  `json:"cpu_idle_jiffies"`
	CPUs            uint64  `json:"cpus"`      // online processor count
	Loadavg1        float64 `json:"loadavg_1"` // /proc/loadavg
	Loadavg5        float64 `json:"loadavg_5"`
	Loadavg15       float64 `json:"loadavg_15"`

	// Memory.
	MemTotalKB     uint64 `json:"mem_total_kb"` // /proc/meminfo
	MemAvailableKB uint64 `json:"mem_available_kb"`
	SwapTotalKB    uint64 `json:"swap_total_kb"`
	SwapFreeKB     uint64 `json:"swap_free_kb"`

	// Disks: one entry per block device in /proc/diskstats (whole disks
	// and partitions alike — the label says which).
	Disks []DiskMetrics `json:"disks,omitempty"`

	// Network: one entry per interface in /proc/net/dev.
	Interfaces []NetMetrics `json:"interfaces,omitempty"`

	// BootUptimeSeconds is /proc/uptime's first field. Lets the UI tell
	// "quiet because rebooting" from "quiet because dead".
	BootUptimeSeconds float64 `json:"boot_uptime_seconds"`
}

// DiskMetrics mirrors one /proc/diskstats row. Fields follow the kernel's
// documented field ordering (fields 3+) — only the widely-used ones.
type DiskMetrics struct {
	// Device is the kernel name (sda, nvme0n1, dm-0, sda1...).
	Device string `json:"device"`
	// Major/Minor identify the device; Device is the human label.
	Major uint64 `json:"major"`
	Minor uint64 `json:"minor"`

	ReadsCompleted  uint64 `json:"reads_completed"`
	ReadSectors     uint64 `json:"read_sectors"` // 512-byte sectors
	WritesCompleted uint64 `json:"writes_completed"`
	WriteSectors    uint64 `json:"write_sectors"` // 512-byte sectors
	IOTicksMs       uint64 `json:"io_ticks_ms"`   // field 13: weighted time doing I/O
}

// NetMetrics mirrors one /proc/net/dev row.
type NetMetrics struct {
	Name  string `json:"name"`
	Bytes struct {
		Rx uint64 `json:"rx"`
		Tx uint64 `json:"tx"`
	} `json:"bytes"`
	Packets struct {
		Rx uint64 `json:"rx"`
		Tx uint64 `json:"tx"`
	} `json:"packets"`
}
