package collect

import (
	"testing"
)

func TestParseLspci_HappyPath(t *testing.T) {
	devs, err := parseLspci(readFixture(t, "lspci-vmmnnk.txt"))
	if err != nil {
		t.Fatalf("parseLspci: %v", err)
	}
	if len(devs) != 4 {
		t.Fatalf("expected 4 devices, got %d", len(devs))
	}
	eth := devs[1]
	if eth.Address != "01:00.0" {
		t.Errorf("Address = %q", eth.Address)
	}
	if eth.ClassID != "0200" {
		t.Errorf("ClassID = %q", eth.ClassID)
	}
	if eth.ClassName != "Ethernet controller" {
		t.Errorf("ClassName = %q", eth.ClassName)
	}
	if eth.VendorID != "8086" {
		t.Errorf("VendorID = %q", eth.VendorID)
	}
	if eth.VendorName != "Intel Corporation" {
		t.Errorf("VendorName = %q", eth.VendorName)
	}
	if eth.DeviceID != "1572" {
		t.Errorf("DeviceID = %q", eth.DeviceID)
	}
	if eth.SubsystemVendor != "1028" {
		t.Errorf("SubsystemVendor = %q", eth.SubsystemVendor)
	}
	if eth.SubsystemDevice != "1f99" {
		t.Errorf("SubsystemDevice = %q", eth.SubsystemDevice)
	}
	if eth.Driver != "i40e" {
		t.Errorf("Driver = %q", eth.Driver)
	}
	if len(eth.KernelModules) != 1 || eth.KernelModules[0] != "i40e" {
		t.Errorf("KernelModules = %v", eth.KernelModules)
	}

	gpu := devs[2]
	if gpu.ClassID != "0300" {
		t.Errorf("gpu ClassID = %q", gpu.ClassID)
	}
	if gpu.DeviceID != "20b0" {
		t.Errorf("gpu DeviceID = %q (device-name brackets should not be mistaken for ID)", gpu.DeviceID)
	}
	if gpu.DeviceName != "GA100 [A100 SXM4 40GB]" {
		t.Errorf("gpu DeviceName = %q (should preserve inner brackets that are part of the name)", gpu.DeviceName)
	}
	if gpu.Driver != "nvidia" {
		t.Errorf("gpu Driver = %q", gpu.Driver)
	}
	if len(gpu.KernelModules) != 3 {
		t.Errorf("gpu KernelModules = %v (expected 3)", gpu.KernelModules)
	}
}

func TestParseLspci_Empty(t *testing.T) {
	devs, err := parseLspci(nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(devs) != 0 {
		t.Errorf("expected 0 devices, got %d", len(devs))
	}
}

func TestParseLspci_SkipsBlockWithoutSlot(t *testing.T) {
	// A trailing block that's just a stray "Driver:" line with no Slot must not
	// produce a phantom device.
	devs, err := parseLspci([]byte("Driver:\tfoo\n"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(devs) != 0 {
		t.Errorf("expected 0 devices, got %d", len(devs))
	}
}

func TestDeriveAccelerators(t *testing.T) {
	devs, err := parseLspci(readFixture(t, "lspci-vmmnnk.txt"))
	if err != nil {
		t.Fatal(err)
	}
	accs := deriveAccelerators(devs)
	if len(accs) != 2 {
		t.Fatalf("expected 2 accelerators (1 gpu + 1 processing), got %d: %+v", len(accs), accs)
	}
	classes := map[string]int{}
	for _, a := range accs {
		classes[a.Class]++
	}
	if classes["gpu"] != 1 {
		t.Errorf("expected 1 gpu, got %d", classes["gpu"])
	}
	if classes["processing"] != 1 {
		t.Errorf("expected 1 processing accelerator, got %d", classes["processing"])
	}
	for _, a := range accs {
		if a.PCIAddress == "" || a.Vendor == "" || a.Model == "" {
			t.Errorf("accelerator missing fields: %+v", a)
		}
	}
}

func TestDeriveAccelerators_None(t *testing.T) {
	if a := deriveAccelerators(nil); len(a) != 0 {
		t.Errorf("expected 0 accelerators, got %d", len(a))
	}
}
