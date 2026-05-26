package collect

import (
	"testing"

	"metalkit/internal/inventory"
)

func TestApplyMcInfo(t *testing.T) {
	b := &inventory.BMC{}
	applyMcInfo(b, readFixture(t, "ipmitool-mc.txt"))
	if b.Vendor != "Dell Inc." {
		t.Errorf("Vendor = %q", b.Vendor)
	}
	if b.ProductID != "256 (0x0100)" {
		t.Errorf("ProductID = %q", b.ProductID)
	}
	if b.FirmwareVersion != "6.10" {
		t.Errorf("FirmwareVersion = %q", b.FirmwareVersion)
	}
	if b.IPMIVersion != "2.0" {
		t.Errorf("IPMIVersion = %q", b.IPMIVersion)
	}
}

func TestApplyMcInfo_Empty(t *testing.T) {
	b := &inventory.BMC{}
	applyMcInfo(b, nil)
	if b.Vendor != "" || b.FirmwareVersion != "" {
		t.Errorf("expected zero values, got %+v", b)
	}
}

func TestApplyLanPrint(t *testing.T) {
	b := &inventory.BMC{}
	applyLanPrint(b, readFixture(t, "ipmitool-lan.txt"))
	if b.MAC != "4c:d9:8f:aa:bb:cc" {
		t.Errorf("MAC = %q", b.MAC)
	}
	if b.IP != "10.20.30.40" {
		t.Errorf("IP = %q", b.IP)
	}
	if b.Gateway != "10.20.30.1" {
		t.Errorf("Gateway = %q", b.Gateway)
	}
	if b.Subnet != "255.255.255.0" {
		t.Errorf("Subnet = %q", b.Subnet)
	}
	if b.VLAN != 0 {
		t.Errorf("VLAN = %d, want 0 for 'Disabled'", b.VLAN)
	}
}

func TestParseFRU(t *testing.T) {
	fru := parseFRU(readFixture(t, "ipmitool-fru.txt"))
	if fru == nil {
		t.Fatal("fru = nil")
	}
	if fru.BoardMfg != "Dell Inc." {
		t.Errorf("BoardMfg = %q", fru.BoardMfg)
	}
	if fru.BoardProduct != "PowerEdge R740" {
		t.Errorf("BoardProduct = %q", fru.BoardProduct)
	}
	if fru.BoardSerial != "CN7475167S00LL" {
		t.Errorf("BoardSerial = %q", fru.BoardSerial)
	}
	if fru.ProductSerial != "ABC1234" {
		t.Errorf("ProductSerial = %q", fru.ProductSerial)
	}
}

func TestParseFRU_Empty(t *testing.T) {
	if fru := parseFRU(nil); fru != nil {
		t.Errorf("expected nil fru for empty input")
	}
}
