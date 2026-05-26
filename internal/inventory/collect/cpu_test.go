package collect

import (
	"testing"
)

func TestParseCPUInfo_HappyPath(t *testing.T) {
	c := parseCPUInfo(readFixture(t, "cpuinfo.txt"))
	if c.TotalThreads != 4 {
		t.Errorf("TotalThreads = %d, want 4", c.TotalThreads)
	}
	if c.Vendor != "GenuineIntel" {
		t.Errorf("Vendor = %q", c.Vendor)
	}
	if len(c.Flags) == 0 {
		t.Errorf("expected non-empty flags")
	}
	if c.Arch == "" {
		t.Errorf("expected Arch to be set")
	}
}

func TestParseCPUInfo_Empty(t *testing.T) {
	c := parseCPUInfo(nil)
	if c.TotalThreads != 0 || c.Vendor != "" || len(c.Flags) != 0 {
		t.Errorf("expected zero-value CPU, got %+v", c)
	}
}

func TestApplyLscpu(t *testing.T) {
	var c = parseCPUInfo(readFixture(t, "cpuinfo.txt"))
	applyLscpu(&c, readFixture(t, "lscpu.json"))
	if c.Sockets != 2 {
		t.Errorf("Sockets = %d, want 2", c.Sockets)
	}
	if c.TotalCores != 48 {
		t.Errorf("TotalCores = %d, want 48 (2*24)", c.TotalCores)
	}
	if c.NUMANodes != 2 {
		t.Errorf("NUMANodes = %d", c.NUMANodes)
	}
}

func TestApplyLscpu_Empty(t *testing.T) {
	c := parseCPUInfo(nil)
	applyLscpu(&c, nil)
	if c.Sockets != 0 || c.TotalCores != 0 {
		t.Errorf("expected zero-value after empty lscpu, got %+v", c)
	}
}

func TestParseProcessors(t *testing.T) {
	procs := parseProcessors(readFixture(t, "dmidecode-processor.txt"))
	if len(procs) != 2 {
		t.Fatalf("expected 2 processors, got %d", len(procs))
	}
	p := procs[0]
	if p.Socket != "CPU1" {
		t.Errorf("Socket = %q", p.Socket)
	}
	if p.Model != "Intel(R) Xeon(R) Gold 6248R CPU @ 3.00GHz" {
		t.Errorf("Model = %q", p.Model)
	}
	if p.Cores != 24 {
		t.Errorf("Cores = %d", p.Cores)
	}
	if p.Threads != 48 {
		t.Errorf("Threads = %d", p.Threads)
	}
	if p.BaseFreqMHz != 3000 {
		t.Errorf("BaseFreqMHz = %d", p.BaseFreqMHz)
	}
	if p.MaxFreqMHz != 4000 {
		t.Errorf("MaxFreqMHz = %d", p.MaxFreqMHz)
	}
	if p.Microcode != "0x5003604" {
		t.Errorf("Microcode = %q", p.Microcode)
	}
}

func TestParseMeminfo(t *testing.T) {
	total, avail := parseMeminfo(readFixture(t, "meminfo.txt"))
	want := uint64(263823672) * 1024
	if total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	wantAvail := uint64(258746204) * 1024
	if avail != wantAvail {
		t.Errorf("avail = %d, want %d", avail, wantAvail)
	}
}

func TestParseMeminfo_Empty(t *testing.T) {
	total, avail := parseMeminfo(nil)
	if total != 0 || avail != 0 {
		t.Errorf("expected zero on empty input, got %d/%d", total, avail)
	}
}
