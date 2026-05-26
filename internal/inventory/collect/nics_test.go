package collect

import (
	"testing"

	"metalkit/internal/inventory"
)

func TestParseIPAddr_HappyPath(t *testing.T) {
	nics, err := parseIPAddr(readFixture(t, "ip-addr.json"))
	if err != nil {
		t.Fatalf("parseIPAddr: %v", err)
	}
	if len(nics) != 2 {
		t.Fatalf("expected 2 nics (loopback skipped), got %d", len(nics))
	}
	up := nics[0]
	if up.Name != "eno1" {
		t.Errorf("Name = %q", up.Name)
	}
	if up.MAC != "f4:ee:08:11:22:33" {
		t.Errorf("MAC = %q", up.MAC)
	}
	if !up.Link {
		t.Errorf("expected Link=true for UP")
	}
	if up.MTU != 1500 {
		t.Errorf("MTU = %d", up.MTU)
	}
	if len(up.Addresses) != 2 {
		t.Errorf("expected 2 addresses, got %d: %v", len(up.Addresses), up.Addresses)
	}
	if up.Addresses[0] != "10.0.0.10/24" {
		t.Errorf("Addresses[0] = %q", up.Addresses[0])
	}
	down := nics[1]
	if down.Link {
		t.Errorf("expected Link=false for DOWN")
	}
}

func TestParseIPAddr_Empty(t *testing.T) {
	nics, err := parseIPAddr([]byte("[]"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(nics) != 0 {
		t.Errorf("expected 0 nics, got %d", len(nics))
	}
}

func TestApplyEthtool(t *testing.T) {
	n := &inventory.NIC{}
	applyEthtool(n, readFixture(t, "ethtool.txt"))
	if n.SpeedMbps != 10000 {
		t.Errorf("SpeedMbps = %d", n.SpeedMbps)
	}
	if n.Duplex != "full" {
		t.Errorf("Duplex = %q", n.Duplex)
	}
}

func TestApplyEthtoolDrvinfo(t *testing.T) {
	n := &inventory.NIC{}
	applyEthtoolDrvinfo(n, readFixture(t, "ethtool-i.txt"))
	if n.Driver != "ixgbe" {
		t.Errorf("Driver = %q", n.Driver)
	}
	if n.DriverVersion != "5.1.0-k" {
		t.Errorf("DriverVersion = %q", n.DriverVersion)
	}
	if n.FirmwareVersion != "0x800009fa, 19.5.12" {
		t.Errorf("FirmwareVersion = %q", n.FirmwareVersion)
	}
	if n.BusInfo != "0000:01:00.0" {
		t.Errorf("BusInfo = %q", n.BusInfo)
	}
	if n.PCIAddress != "0000:01:00.0" {
		t.Errorf("PCIAddress = %q", n.PCIAddress)
	}
}

func TestParseEthtoolModule(t *testing.T) {
	sfp := parseEthtoolModule(readFixture(t, "ethtool-m.txt"))
	if sfp == nil {
		t.Fatal("sfp = nil")
	}
	if sfp.Vendor != "FINISAR CORP." {
		t.Errorf("Vendor = %q", sfp.Vendor)
	}
	if sfp.PartNumber != "FTLX8574D3BCL" {
		t.Errorf("PartNumber = %q", sfp.PartNumber)
	}
	if sfp.Serial != "ABCD1234" {
		t.Errorf("Serial = %q", sfp.Serial)
	}
	if sfp.WavelengthNM != 850 {
		t.Errorf("WavelengthNM = %d", sfp.WavelengthNM)
	}
}

func TestParseEthtoolModule_Empty(t *testing.T) {
	if sfp := parseEthtoolModule(nil); sfp != nil {
		t.Errorf("expected nil sfp for empty input")
	}
}
