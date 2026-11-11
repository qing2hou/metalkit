package collect

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUEFIModeFromPath(t *testing.T) {
	dir := t.TempDir()
	efi := filepath.Join(dir, "efivars")
	if err := os.MkdirAll(efi, 0o755); err != nil {
		t.Fatal(err)
	}
	if !uefiModeFromPath(efi) {
		t.Error("directory should report UEFI")
	}
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if uefiModeFromPath(file) {
		t.Error("regular file must not report UEFI")
	}
	if uefiModeFromPath(filepath.Join(dir, "missing")) {
		t.Error("missing path must report BIOS/unknown")
	}
}

func TestParseUptime(t *testing.T) {
	cases := []struct {
		in  string
		out int64
	}{
		{"12345.67 9876.54\n", 12345},
		{"0.01 0.01\n", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := parseUptime([]byte(c.in)); got != c.out {
			t.Errorf("parseUptime(%q) = %d, want %d", c.in, got, c.out)
		}
	}
}

func TestParseMounts_HappyPath(t *testing.T) {
	mounts := parseMounts(readFixture(t, "mounts.txt"))
	// Should skip proc/sysfs/tmpfs but keep ext4/xfs entries.
	var sources []string
	for _, m := range mounts {
		sources = append(sources, m.Source)
	}
	if len(mounts) != 3 {
		t.Fatalf("expected 3 mounts (ext4 root, ext4 boot, xfs data), got %d: %v", len(mounts), sources)
	}
	if mounts[0].Source != "/dev/sda1" || mounts[0].Target != "/" || mounts[0].FSType != "ext4" {
		t.Errorf("mounts[0] = %+v", mounts[0])
	}
	if mounts[2].FSType != "xfs" {
		t.Errorf("mounts[2] fstype = %q", mounts[2].FSType)
	}
}

func TestParseMounts_Empty(t *testing.T) {
	if m := parseMounts(nil); len(m) != 0 {
		t.Errorf("expected 0 mounts, got %d", len(m))
	}
}
