// collect.go reads one Metrics sample out of /proc. All access goes through
// a small FS abstraction so tests can feed crafted file contents; the
// production implementation is plain os.ReadFile.
//
// Every collector is best-effort: a missing or unparsable file contributes
// zero values to the sample instead of failing it. A monitoring agent that
// dies because a file moved is worse than one reporting partial data.
package monitor

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// FS is the slice of the filesystem the collectors touch.
type FS interface {
	ReadFile(name string) ([]byte, error)
}

// OSFS implements FS over the real filesystem.
type OSFS struct{}

func (OSFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// Collect gathers one Metrics sample using the given filesystem.
// fs may be nil — it then defaults to OSFS.
func Collect(fs FS) Metrics {
	if fs == nil {
		fs = OSFS{}
	}
	return Metrics{
		CPUs:              collectCPUCount(fs),
		CPUTotalJiffies:   cpuStat(fs).total,
		CPUIdleJiffies:    cpuStat(fs).idle,
		Loadavg1:          collectLoadavg(fs)[0],
		Loadavg5:          collectLoadavg(fs)[1],
		Loadavg15:         collectLoadavg(fs)[2],
		MemTotalKB:        meminfo(fs)["MemTotal"],
		MemAvailableKB:    meminfo(fs)["MemAvailable"],
		SwapTotalKB:       meminfo(fs)["SwapTotal"],
		SwapFreeKB:        meminfo(fs)["SwapFree"],
		Disks:             collectDisks(fs),
		Interfaces:        collectInterfaces(fs),
		BootUptimeSeconds: collectUptime(fs),
	}
}

// CollectPayload wraps Collect into a wire Payload. machineUUID comes from
// the caller (the monitor main gets it from dmidecode once at startup).
func CollectPayload(fs FS, machineUUID string) Payload {
	return Payload{
		MachineUUID:   machineUUID,
		SchemaVersion: SchemaVersion,
		MonitoredAt:   time.Now().UTC(),
		Metrics:       Collect(fs),
	}
}

// collectCPUCount counts online CPUs via /sys/devices/system/cpu/online.
// On partial failure it falls back to counting /proc/stat cpuN lines.
func collectCPUCount(fs FS) uint64 {
	if b, err := fs.ReadFile("/sys/devices/system/cpu/online"); err == nil {
		if n := countCPUList(string(b)); n > 0 {
			return n
		}
	}
	if b, err := fs.ReadFile("/proc/stat"); err == nil {
		n := uint64(0)
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "cpu") && !strings.HasPrefix(line, "cpu ") {
				n++
			}
		}
		return n
	}
	return 0
}

// countCPUList counts processors in a kernel CPU list ("0-3,8,10-11" → 7).
func countCPUList(s string) uint64 {
	var n uint64
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, ranged := strings.Cut(part, "-")
		loN, err := strconv.ParseUint(strings.TrimSpace(lo), 10, 32)
		if err != nil {
			continue
		}
		if !ranged {
			n++
			continue
		}
		hiN, err := strconv.ParseUint(strings.TrimSpace(hi), 10, 32)
		if err != nil || hiN < loN {
			continue
		}
		n += hiN - loN + 1
	}
	return n
}

// cpuStat reads the aggregate "cpu " line of /proc/stat. Returned jiffies
// are the sum across all fields except guest (visitor double counting —
// guest time is already included in user/nice) and the idle count includes
// iowait for the same reason the kernel reports them separately but
// monitoring usually wants both.
type cpuSample struct {
	total uint64
	idle  uint64
}

func cpuStat(fs FS) cpuSample {
	var s cpuSample
	b, err := fs.ReadFile("/proc/stat")
	if err != nil {
		return s
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var vals []uint64
		for _, f := range fields {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				break
			}
			vals = append(vals, v)
		}
		// /proc/stat order: user nice system idle iowait irq softirq steal
		// guest guest_nice. Aggregate view keeps the first 8.
		if len(vals) > 8 {
			vals = vals[:8]
		}
		for _, v := range vals {
			s.total += v
		}
		// idle + iowait (if present) counts as "not busy".
		if len(vals) > 3 {
			s.idle = vals[3]
		}
		if len(vals) > 4 {
			s.idle += vals[4]
		}
		break
	}
	return s
}

// collectLoadavg reads the three classic load averages.
func collectLoadavg(fs FS) [3]float64 {
	var out [3]float64
	b, err := fs.ReadFile("/proc/loadavg")
	if err != nil {
		return out
	}
	fields := strings.Fields(string(b))
	for i := 0; i < 3 && i < len(fields); i++ {
		if v, err := strconv.ParseFloat(fields[i], 64); err == nil {
			out[i] = v
		}
	}
	return out
}

// meminfo parses /proc/meminfo's "Key: 123 kB" lines into a map. Only the
// keys Metrics cares about are kept; unknown keys are ignored (the file
// has ~50 lines on modern kernels).
func meminfo(fs FS) map[string]uint64 {
	out := map[string]uint64{}
	b, err := fs.ReadFile("/proc/meminfo")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		out[strings.TrimSpace(key)] = v
	}
	return out
}

// collectDisks parses /proc/diskstats. Field layout (kernel docs
// Documentation/abi-testing/procfs-diskstats): major minor name
// reads_completed reads_merged read_sectors ms_reading writes_completed
// writes_merged write_sectors ms_writing ios_in_flight ms_doing_io
// weighted_ms_doing_io ...
func collectDisks(fs FS) []DiskMetrics {
	b, err := fs.ReadFile("/proc/diskstats")
	if err != nil {
		return nil
	}
	var out []DiskMetrics
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 14 {
			continue
		}
		major, _ := strconv.ParseUint(f[0], 10, 32)
		minor, _ := strconv.ParseUint(f[1], 10, 32)
		reads, _ := strconv.ParseUint(f[3], 10, 64)
		readSec, _ := strconv.ParseUint(f[5], 10, 64)
		writes, _ := strconv.ParseUint(f[7], 10, 64)
		writeSec, _ := strconv.ParseUint(f[9], 10, 64)
		ioTicks, _ := strconv.ParseUint(f[12], 10, 64)
		out = append(out, DiskMetrics{
			Device:          f[2],
			Major:           major,
			Minor:           minor,
			ReadsCompleted:  reads,
			ReadSectors:     readSec,
			WritesCompleted: writes,
			WriteSectors:    writeSec,
			IOTicksMs:       ioTicks,
		})
	}
	return out
}

// collectInterfaces parses /proc/net/dev.
func collectInterfaces(fs FS) []NetMetrics {
	b, err := fs.ReadFile("/proc/net/dev")
	if err != nil {
		return nil
	}
	var out []NetMetrics
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || name == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			continue
		}
		var m NetMetrics
		m.Name = name
		m.Bytes.Rx, _ = strconv.ParseUint(f[0], 10, 64)
		m.Packets.Rx, _ = strconv.ParseUint(f[1], 10, 64)
		m.Bytes.Tx, _ = strconv.ParseUint(f[8], 10, 64)
		m.Packets.Tx, _ = strconv.ParseUint(f[9], 10, 64)
		out = append(out, m)
	}
	return out
}

// collectUptime reads /proc/uptime's first field.
func collectUptime(fs FS) float64 {
	b, err := fs.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return v
}
