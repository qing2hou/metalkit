package collect

import (
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestParseSystemInfo_HappyPath(t *testing.T) {
	m, err := parseSystemInfo(readFixture(t, "dmidecode-system.txt"))
	if err != nil {
		t.Fatalf("parseSystemInfo: %v", err)
	}
	if m.Manufacturer != "Dell Inc." {
		t.Errorf("Manufacturer = %q, want Dell Inc.", m.Manufacturer)
	}
	if m.ProductName != "PowerEdge R740" {
		t.Errorf("ProductName = %q", m.ProductName)
	}
	if m.Version != "" { // "Not Specified" → cleaned to empty
		t.Errorf("Version = %q, want empty (Not Specified)", m.Version)
	}
	if m.Serial != "ABC1234" {
		t.Errorf("Serial = %q", m.Serial)
	}
	if m.SMBIOSUUID != "4c4c4544-0042-4310-8043-c8c04f303432" {
		t.Errorf("SMBIOSUUID = %q", m.SMBIOSUUID)
	}
	if m.SKU != "SKU=NotProvided;ModelName=PowerEdge R740" {
		t.Errorf("SKU = %q", m.SKU)
	}
	if m.Family != "PowerEdge" {
		t.Errorf("Family = %q", m.Family)
	}
}

func TestParseSystemInfo_Empty(t *testing.T) {
	m, err := parseSystemInfo(nil)
	if err != nil {
		t.Fatalf("parseSystemInfo(nil): %v", err)
	}
	if m.Manufacturer != "" || m.ProductName != "" || m.SMBIOSUUID != "" {
		t.Errorf("expected zero-value Machine, got %+v", m)
	}
}

func TestParseBaseboard(t *testing.T) {
	bb := parseBaseboard(readFixture(t, "dmidecode-baseboard.txt"))
	if bb.Manufacturer != "Dell Inc." {
		t.Errorf("Manufacturer = %q", bb.Manufacturer)
	}
	if bb.Product != "0YWR7D" {
		t.Errorf("Product = %q", bb.Product)
	}
	if bb.Version != "A07" {
		t.Errorf("Version = %q", bb.Version)
	}
	if bb.Serial != ".ABC1234.CN7475167S00LL." {
		t.Errorf("Serial = %q", bb.Serial)
	}
}

func TestParseBaseboard_Empty(t *testing.T) {
	bb := parseBaseboard(nil)
	if bb.Manufacturer != "" || bb.Product != "" {
		t.Errorf("expected zero-value Baseboard, got %+v", bb)
	}
}

func TestParseChassis(t *testing.T) {
	c := parseChassis(readFixture(t, "dmidecode-chassis.txt"))
	if c.Manufacturer != "Dell Inc." {
		t.Errorf("Manufacturer = %q", c.Manufacturer)
	}
	if c.Type != "Rack Mount Chassis" {
		t.Errorf("Type = %q", c.Type)
	}
	if c.Serial != "ABC1234" {
		t.Errorf("Serial = %q", c.Serial)
	}
}

func TestParseChassis_Empty(t *testing.T) {
	c := parseChassis(nil)
	if c.Type != "" {
		t.Errorf("expected zero-value Chassis, got %+v", c)
	}
}

func TestParseBIOS(t *testing.T) {
	b := parseBIOS(readFixture(t, "dmidecode-bios.txt"))
	if b.Vendor != "Dell Inc." {
		t.Errorf("Vendor = %q", b.Vendor)
	}
	if b.Version != "2.19.1" {
		t.Errorf("Version = %q", b.Version)
	}
	if b.ReleaseDate != "04/01/2024" {
		t.Errorf("ReleaseDate = %q", b.ReleaseDate)
	}
	if b.Revision != "2.19" {
		t.Errorf("Revision = %q", b.Revision)
	}
}

func TestParseBIOS_Empty(t *testing.T) {
	b := parseBIOS(nil)
	if b.Vendor != "" || b.Version != "" {
		t.Errorf("expected zero-value BIOS, got %+v", b)
	}
}

func TestParseMemoryArrayECC(t *testing.T) {
	if !parseMemoryArrayECC(readFixture(t, "dmidecode-memarray.txt")) {
		t.Errorf("expected ECC=true for 'Multi-bit ECC'")
	}
	if parseMemoryArrayECC(nil) {
		t.Errorf("expected ECC=false on empty input")
	}
}

func TestParseDIMMs(t *testing.T) {
	dimms := parseDIMMs(readFixture(t, "dmidecode-dimms.txt"))
	if len(dimms) != 2 {
		t.Fatalf("expected 2 populated DIMMs, got %d", len(dimms))
	}
	d := dimms[0]
	if d.Locator != "A1" {
		t.Errorf("Locator = %q", d.Locator)
	}
	if d.SizeBytes != 32*1024*1024*1024 {
		t.Errorf("SizeBytes = %d, want %d", d.SizeBytes, uint64(32)*1024*1024*1024)
	}
	if d.Type != "DDR4" {
		t.Errorf("Type = %q", d.Type)
	}
	if d.SpeedMTS != 2933 {
		t.Errorf("SpeedMTS = %d", d.SpeedMTS)
	}
	if d.ConfiguredSpeedMTS != 2666 {
		t.Errorf("ConfiguredSpeedMTS = %d", d.ConfiguredSpeedMTS)
	}
	if d.Manufacturer != "Samsung" {
		t.Errorf("Manufacturer = %q", d.Manufacturer)
	}
	if d.Serial != "12345ABC" {
		t.Errorf("Serial = %q", d.Serial)
	}
	if d.PartNumber != "M393A4K40CB2-CVF" {
		t.Errorf("PartNumber = %q", d.PartNumber)
	}
	if d.Rank != 2 {
		t.Errorf("Rank = %d", d.Rank)
	}
	if d.VoltageV != "1.2 V" {
		t.Errorf("VoltageV = %q", d.VoltageV)
	}
}

func TestParseDIMMs_Empty(t *testing.T) {
	d := parseDIMMs(nil)
	if len(d) != 0 {
		t.Errorf("expected 0 DIMMs, got %d", len(d))
	}
}

func TestParseDIMMSize(t *testing.T) {
	cases := []struct {
		in  string
		out uint64
	}{
		{"32 GB", 32 * 1024 * 1024 * 1024},
		{"16384 MB", 16384 * 1024 * 1024},
		{"8 GiB", 8 * 1024 * 1024 * 1024},
		{"No Module Installed", 0},
		{"", 0},
	}
	for _, c := range cases {
		got := parseDIMMSize(c.in)
		if got != c.out {
			t.Errorf("parseDIMMSize(%q) = %d, want %d", c.in, got, c.out)
		}
	}
}
