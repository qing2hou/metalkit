package collect

import (
	"testing"
)

func TestParseLsblk_HappyPath(t *testing.T) {
	disks, err := parseLsblk(readFixture(t, "lsblk.json"))
	if err != nil {
		t.Fatalf("parseLsblk: %v", err)
	}
	if len(disks) != 2 {
		t.Fatalf("expected 2 disks (excluding sr0/part), got %d", len(disks))
	}
	sda := disks[0]
	if sda.KName != "sda" {
		t.Errorf("KName = %q", sda.KName)
	}
	if sda.Path != "/dev/sda" {
		t.Errorf("Path = %q", sda.Path)
	}
	if sda.SizeBytes != 480103981056 {
		t.Errorf("SizeBytes = %d", sda.SizeBytes)
	}
	if sda.Transport != "sata" {
		t.Errorf("Transport = %q", sda.Transport)
	}
	if sda.Rotational {
		t.Errorf("expected Rotational=false")
	}
	if sda.Removable {
		t.Errorf("expected Removable=false")
	}
	nvme := disks[1]
	if nvme.Transport != "nvme" {
		t.Errorf("nvme Transport = %q", nvme.Transport)
	}
}

func TestParseLsblk_Empty(t *testing.T) {
	disks, err := parseLsblk([]byte(`{"blockdevices":[]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(disks) != 0 {
		t.Errorf("expected 0 disks, got %d", len(disks))
	}
}

func TestParseSmartctl_SATA(t *testing.T) {
	s := parseSmartctl(readFixture(t, "smartctl-sata.json"))
	if s == nil {
		t.Fatal("smart = nil")
	}
	if s.Health != "PASSED" {
		t.Errorf("Health = %q", s.Health)
	}
	if s.PowerOnHours != 12345 {
		t.Errorf("PowerOnHours = %d", s.PowerOnHours)
	}
	if s.PowerCycles != 42 {
		t.Errorf("PowerCycles = %d", s.PowerCycles)
	}
	if s.TemperatureC != 31 {
		t.Errorf("TemperatureC = %d", s.TemperatureC)
	}
	if s.PendingSectors != 2 {
		t.Errorf("PendingSectors = %d", s.PendingSectors)
	}
	if s.BytesWritten != 1000*512 {
		t.Errorf("BytesWritten = %d", s.BytesWritten)
	}
	if s.BytesRead != 500*512 {
		t.Errorf("BytesRead = %d", s.BytesRead)
	}
}

func TestParseSmartctl_NVMe(t *testing.T) {
	s := parseSmartctl(readFixture(t, "smartctl-nvme.json"))
	if s == nil {
		t.Fatal("smart = nil")
	}
	if s.Health != "PASSED" {
		t.Errorf("Health = %q", s.Health)
	}
	if s.BytesRead != 100*512_000 {
		t.Errorf("BytesRead = %d", s.BytesRead)
	}
	if s.BytesWritten != 200*512_000 {
		t.Errorf("BytesWritten = %d", s.BytesWritten)
	}
}

func TestParseSmartctl_Empty(t *testing.T) {
	if s := parseSmartctl([]byte("{}")); s == nil {
		t.Errorf("expected non-nil SMART even on empty JSON")
	}
	if s := parseSmartctl([]byte("not json")); s != nil {
		t.Errorf("expected nil on invalid JSON")
	}
}

func TestParseNvmeIdCtrl(t *testing.T) {
	if got := parseNvmeIdCtrl(readFixture(t, "nvme-id-ctrl.json")); got != 1 {
		t.Errorf("namespaces = %d, want 1", got)
	}
}

func TestParseNvmeSmartLog(t *testing.T) {
	if got := parseNvmeSmartLog(readFixture(t, "nvme-smart-log.json")); got != 45 {
		t.Errorf("max temp = %d, want 45 (318K - 273)", got)
	}
}
