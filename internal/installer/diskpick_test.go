package installer

import (
	"strings"
	"testing"

	"metalkit/internal/profiles"
)

func sampleDisks() []Disk {
	return []Disk{
		{Name: "sda", DevPath: "/dev/sda", SizeBytes: 500 * 1e9, Transport: "sata",
			Model: "WDC1TB", WWN: "0x5000c500abc", ByPath: "pci-0000:00:17.0-ata-1"},
		{Name: "sdb", DevPath: "/dev/sdb", SizeBytes: 250 * 1e9, Transport: "sata",
			Model: "Samsung250", WWN: "0x5000c500def", ByPath: "pci-0000:00:17.0-ata-2"},
		{Name: "sdc", DevPath: "/dev/sdc", SizeBytes: 16 * 1e9, Removable: true, Transport: "usb",
			Model: "SanDiskUSB", WWN: "", ByPath: "pci-0000:00:14.0-usb-1"},
		{Name: "sdd", DevPath: "/dev/sdd", SizeBytes: 100 * 1e9, ReadOnly: true, Transport: "sata",
			Model: "ReadOnlyDisk", WWN: "0x5000c500ro1", ByPath: "pci-0000:00:17.0-ata-3"},
		{Name: "nvme0n1", DevPath: "/dev/nvme0n1", SizeBytes: 1 * 1e12, Transport: "nvme",
			Model: "SamsungNVMe", WWN: "nvme.abc123", ByPath: "pci-0000:01:00.0-nvme-1"},
		{Name: "sde", DevPath: "/dev/sde", SizeBytes: 0, Transport: "sata",
			Model: "EmptyCardReader", WWN: ""},
	}
}

func TestPickDisk_Smallest(t *testing.T) {
	got, err := PickDisk(sampleDisks(), profiles.TargetDisk{Mode: "smallest"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Name != "sdb" {
		t.Fatalf("want sdb (250GB, fixed, non-RO, non-USB), got %s (%d bytes)", got.Name, got.SizeBytes)
	}
}

func TestPickDisk_Smallest_EmptyDefault(t *testing.T) {
	// mode="" (default) should behave like smallest
	got, err := PickDisk(sampleDisks(), profiles.TargetDisk{Mode: ""})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Name != "sdb" {
		t.Fatalf("default mode should pick smallest sdb, got %s", got.Name)
	}
}

func TestPickDisk_Smallest_NoCandidates(t *testing.T) {
	disks := []Disk{
		{Name: "sdc", Removable: true, SizeBytes: 16 * 1e9, Transport: "usb"},
	}
	_, err := PickDisk(disks, profiles.TargetDisk{Mode: "smallest"})
	if err == nil {
		t.Fatal("expected disk-not-found error")
	}
	if !strings.Contains(err.Error(), "disk not found") {
		t.Fatalf("error %q must contain 'disk not found'", err)
	}
}

func TestPickDisk_ByPath(t *testing.T) {
	got, err := PickDisk(sampleDisks(),
		profiles.TargetDisk{Mode: "by-path", Value: "pci-0000:01:00.0-nvme-1"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Name != "nvme0n1" {
		t.Fatalf("want nvme0n1, got %s", got.Name)
	}
}

func TestPickDisk_ByPath_NotFound(t *testing.T) {
	_, err := PickDisk(sampleDisks(),
		profiles.TargetDisk{Mode: "by-path", Value: "pci-0000:99:99.0-bogus"})
	if err == nil || !strings.Contains(err.Error(), "disk not found") {
		t.Fatalf("expected 'disk not found' error, got %v", err)
	}
}

func TestPickDisk_ByWWN(t *testing.T) {
	got, err := PickDisk(sampleDisks(),
		profiles.TargetDisk{Mode: "by-wwn", Value: "0x5000c500abc"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Name != "sda" {
		t.Fatalf("want sda by WWN, got %s", got.Name)
	}
}

func TestPickDisk_ByModel(t *testing.T) {
	got, err := PickDisk(sampleDisks(),
		profiles.TargetDisk{Mode: "by-model", Value: "Samsung250"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Name != "sdb" {
		t.Fatalf("want sdb by Model, got %s", got.Name)
	}
}

func TestPickDisk_EmptyValueForByMode(t *testing.T) {
	for _, mode := range []string{"by-path", "by-wwn", "by-model"} {
		t.Run(mode, func(t *testing.T) {
			_, err := PickDisk(sampleDisks(),
				profiles.TargetDisk{Mode: mode, Value: ""})
			if err == nil {
				t.Fatalf("mode=%s with empty value must error", mode)
			}
		})
	}
}

func TestPickDisk_UnknownMode(t *testing.T) {
	_, err := PickDisk(sampleDisks(),
		profiles.TargetDisk{Mode: "by-uuid", Value: "x"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported-mode error, got %v", err)
	}
}

func TestPickDisk_NoCandidatesAtAll(t *testing.T) {
	_, err := PickDisk(nil, profiles.TargetDisk{Mode: "smallest"})
	if err == nil || !strings.Contains(err.Error(), "disk not found") {
		t.Fatalf("expected disk-not-found, got %v", err)
	}
}
