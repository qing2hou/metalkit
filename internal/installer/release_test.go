package installer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// seedReleaseMocks wires the commands releaseDisk depends on:
//   - /dev/sda1 mounted at /boot            → must umount
//   - /dev/sda3 active swap                 → must swapoff
//   - /dev/sdb1 mounted + swap elsewhere     → must be left alone
//   - VG "vg0" has its PV on /dev/sda3        → vgchange -an vg0
//   - VG "othervg" on /dev/sdb1               → untouched
//   - /dev/md0 (raid1) stacked on the disk    → mdadm --stop
//   - /dev/mapper/cryptroot (crypt)           → cryptsetup close
func seedReleaseMocks(m *mockExec) {
	// Deepest children first, as `lsblk -r` prints them.
	m.On["lsblk"] = mockExecResult{Out: []byte(strings.Join([]string{
		"/dev/sda disk",
		"/dev/mapper/cryptroot crypt",
		"/dev/sda3 part",
		"/dev/md0 raid1",
		"/dev/sda1 part /boot",
	}, "\n") + "\n")}
	m.On["cat"] = mockExecResult{Out: []byte(strings.Join([]string{
		"Filename                                Type            Size    Used    Priority",
		"/dev/sda3                               partition       8388604 512     -2",
		"/dev/sdb2                               partition       4194300 0       -3",
	}, "\n") + "\n")}
	m.On["pvs"] = mockExecResult{Out: []byte(strings.Join([]string{
		"  /dev/sda3 vg0",
		"  /dev/sdb1 othervg",
	}, "\n") + "\n")}
}

func TestReleaseDiskActions(t *testing.T) {
	m := newMockExec()
	seedReleaseMocks(m)
	deps := Deps{Exec: m, Logger: testLogger(t)}

	released := releaseDisk(context.Background(), deps, "/dev/sda")
	joined := strings.Join(released, "; ")
	for _, want := range []string{
		"umount /dev/sda1",
		"swapoff /dev/sda3",
		"vgchange -an vg0",
		"mdadm --stop /dev/md0",
		"cryptsetup close cryptroot",
	} {
		found := false
		for _, r := range released {
			if r == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in released actions, got: %s", want, joined)
		}
	}

	// Nothing on the OTHER disk may be touched.
	for _, c := range m.calls {
		line := c.Name + " " + strings.Join(c.Args, " ")
		if strings.Contains(line, "sdb") {
			t.Errorf("release touched another disk: %s", line)
		}
		if strings.Contains(line, "othervg") {
			t.Errorf("release touched another VG: %s", line)
		}
	}
}

// Only mounted entries trigger umount; a clean disk releases nothing.
func TestReleaseDiskCleanDisk(t *testing.T) {
	m := newMockExec()
	m.On["lsblk"] = mockExecResult{Out: []byte("/dev/sda disk\n/dev/sda1 part\n")}
	m.On["cat"] = mockExecResult{Out: []byte("Filename Type Size Used Priority\n")}
	m.On["pvs"] = mockExecResult{Out: []byte("")}
	deps := Deps{Exec: m, Logger: testLogger(t)}

	if got := releaseDisk(context.Background(), deps, "/dev/sda"); len(got) != 0 {
		t.Errorf("clean disk released %v, want nothing", got)
	}
	for _, c := range m.calls {
		switch c.Name {
		case "umount", "swapoff", "vgchange", "mdadm", "cryptsetup":
			t.Errorf("unexpected destructive call on clean disk: %s %v", c.Name, c.Args)
		}
	}
}

// A disk with no lsblk output (e.g. lsblk missing) must degrade quietly.
func TestReleaseDiskToolingMissing(t *testing.T) {
	m := newMockExec()
	m.On["lsblk"] = mockExecResult{Err: errors.New("not found")}
	m.On["cat"] = mockExecResult{Err: errors.New("not found")}
	m.On["pvs"] = mockExecResult{Err: errors.New("not found")}
	deps := Deps{Exec: m, Logger: testLogger(t)}

	if got := releaseDisk(context.Background(), deps, "/dev/sda"); len(got) != 0 {
		t.Errorf("released %v with no tooling, want nothing", got)
	}
}
