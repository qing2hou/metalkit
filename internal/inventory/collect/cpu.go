package collect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"metalkit/internal/inventory"
)

func collectCPU(ctx context.Context, r *inventory.Report) error {
	cpuinfo, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return fmt.Errorf("read /proc/cpuinfo: %w", err)
	}
	cpu := parseCPUInfo(cpuinfo)

	if out, err := runCmd(ctx, 8*time.Second, "lscpu", "-J"); err == nil {
		applyLscpu(&cpu, out)
	}
	if proc, err := runCmd(ctx, 8*time.Second, "dmidecode", "-t", "processor"); err == nil {
		cpu.PerSocket = parseProcessors(proc)
	}
	// Microcode: prefer per-socket field from dmidecode, fallback to sysfs for socket 0.
	if mc, err := os.ReadFile("/sys/devices/system/cpu/cpu0/microcode/version"); err == nil {
		for i := range cpu.PerSocket {
			if cpu.PerSocket[i].Microcode == "" {
				cpu.PerSocket[i].Microcode = strings.TrimSpace(string(mc))
			}
		}
	}
	r.CPU = cpu
	return nil
}

func parseCPUInfo(data []byte) inventory.CPU {
	var c inventory.CPU
	c.Arch = runtime.GOARCH
	sc := bufio.NewScanner(bytes.NewReader(data))
	gotFlags := false
	for sc.Scan() {
		line := sc.Text()
		key, val, ok := splitKV(line)
		if !ok {
			continue
		}
		switch key {
		case "processor":
			c.TotalThreads++
		case "vendor_id":
			if c.Vendor == "" {
				c.Vendor = val
			}
		case "flags":
			if !gotFlags {
				c.Flags = strings.Fields(val)
				gotFlags = true
			}
		}
	}
	return c
}

// splitKV parses "key : value" /proc/cpuinfo-style lines.
func splitKV(line string) (string, string, bool) {
	colon := strings.Index(line, ":")
	if colon < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:colon]), strings.TrimSpace(line[colon+1:]), true
}

// lscpu -J JSON shape: { "lscpu": [ {"field":"…", "data":"…", "children":[…]} … ] }
type lscpuEntry struct {
	Field    string       `json:"field"`
	Data     string       `json:"data"`
	Children []lscpuEntry `json:"children,omitempty"`
}
type lscpuOut struct {
	Lscpu []lscpuEntry `json:"lscpu"`
}

func applyLscpu(c *inventory.CPU, out []byte) {
	var l lscpuOut
	if err := json.Unmarshal(out, &l); err != nil {
		return
	}
	flat := map[string]string{}
	var walk func([]lscpuEntry)
	walk = func(es []lscpuEntry) {
		for _, e := range es {
			key := strings.TrimSuffix(strings.TrimSpace(e.Field), ":")
			flat[key] = strings.TrimSpace(e.Data)
			if len(e.Children) > 0 {
				walk(e.Children)
			}
		}
	}
	walk(l.Lscpu)

	if v := flat["Socket(s)"]; v != "" {
		c.Sockets = atoiSafe(v)
	}
	coresPerSocket := atoiSafe(flat["Core(s) per socket"])
	if c.Sockets > 0 && coresPerSocket > 0 {
		c.TotalCores = c.Sockets * coresPerSocket
	}
	if v := flat["NUMA node(s)"]; v != "" {
		c.NUMANodes = atoiSafe(v)
	}
	if v := flat["Vendor ID"]; v != "" && c.Vendor == "" {
		c.Vendor = v
	}
}

func parseProcessors(out []byte) []inventory.CPUEntry {
	var entries []inventory.CPUEntry
	for _, blk := range parseDMIBlocks(out) {
		if blk.dmiType != 4 {
			continue
		}
		status := strings.ToLower(blk.kv["Status"])
		if strings.Contains(status, "unpopulated") {
			continue
		}
		e := inventory.CPUEntry{
			Socket:      dmiClean(blk.kv["Socket Designation"]),
			Model:       dmiClean(blk.kv["Version"]),
			Cores:       atoiSafe(blk.kv["Core Count"]),
			Threads:     atoiSafe(blk.kv["Thread Count"]),
			BaseFreqMHz: parseFreqMHz(blk.kv["Current Speed"]),
			MaxFreqMHz:  parseFreqMHz(blk.kv["Max Speed"]),
			Microcode:   dmiClean(blk.kv["Microcode"]),
		}
		entries = append(entries, e)
	}
	return entries
}

func parseFreqMHz(s string) int {
	// dmidecode reports e.g. "2400 MHz".
	fields := strings.Fields(s)
	if len(fields) < 1 {
		return 0
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0
	}
	return n
}
