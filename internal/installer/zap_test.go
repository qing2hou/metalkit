package installer

import (
	"context"
	"strings"
	"testing"
)

func TestWipeDisk_CallsSgdiskZ(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["sgdisk -Z /dev/sda"] = mockExecResult{Out: []byte("GPT data structures destroyed\n")}
	rep := &mockReporter{}
	deps := Deps{Exec: exec, FS: newMockFS(), Reporter: rep}

	wipeDisk(context.Background(), deps, "/dev/sda")

	calls := exec.Calls()
	var saw bool
	for _, c := range calls {
		if c.Name == "sgdisk" && len(c.Args) >= 2 && c.Args[0] == "-Z" && c.Args[1] == "/dev/sda" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("sgdisk -Z /dev/sda not called; calls=%v", calls)
	}
	// Success should be logged at info level.
	var sawInfo bool
	for _, l := range rep.logs {
		if strings.HasPrefix(l, "info:") && strings.Contains(l, "zapped partition table") {
			sawInfo = true
		}
	}
	if !sawInfo {
		t.Fatalf("expected info log about zapped partition table; logs=%v", rep.logs)
	}
}

func TestWipeDisk_FailureDoesNotAbort(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["sgdisk -Z /dev/sda"] = mockExecResult{Err: errString("sgdisk not found")}
	rep := &mockReporter{}
	deps := Deps{Exec: exec, FS: newMockFS(), Reporter: rep}

	// wipeDisk is best-effort: it must not return an error or panic.
	wipeDisk(context.Background(), deps, "/dev/sda")

	var sawWarn bool
	for _, l := range rep.logs {
		if strings.HasPrefix(l, "warn:") && strings.Contains(l, "sgdisk -Z") {
			sawWarn = true
		}
	}
	if !sawWarn {
		t.Fatalf("expected warn log on sgdisk failure; logs=%v", rep.logs)
	}
}

func TestCollectPartUUIDs_ParsesLsblk(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnbo NAME,PARTUUID /dev/sda"] = mockExecResult{Out: []byte(
		"/dev/sda1 db45e66a-cfff-4f33-8847-bbaedcbd32d6\n" +
			"/dev/sda2 49e639e8-0023-48ef-9990-f55882e78674\n" +
			"/dev/sda3 \n",
	)}
	deps := Deps{Exec: exec, FS: newMockFS()}

	uuids := collectPartUUIDs(context.Background(), deps, "/dev/sda")
	if len(uuids) != 2 {
		t.Fatalf("expected 2 PARTUUIDs, got %d (%v)", len(uuids), uuids)
	}
	// All returned UUIDs must be lowercased for comparison with NVRAM.
	for _, u := range uuids {
		if u != strings.ToLower(u) {
			t.Fatalf("PARTUUID %q not lowercased", u)
		}
	}
}

func TestCollectPartUUIDs_LsblkFailureReturnsNil(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnbo NAME,PARTUUID /dev/sda"] = mockExecResult{Err: errString("no disk")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	if uuids := collectPartUUIDs(context.Background(), deps, "/dev/sda"); uuids != nil {
		t.Fatalf("expected nil on lsblk failure, got %v", uuids)
	}
}

func TestPruneStaleNVRAM_DeletesStaleGPTEntries(t *testing.T) {
	exec := newMockExec()
	// Post-write disk has partitions with these GUIDs (the fresh
	// openEuler ESP + the new Debian ESP just created by grub-install).
	currentESP := "4469adb0-1041-490e-aab2-da41479c1d34"
	currentRoot := "11111111-2222-3333-4444-555555555555"
	stale1 := "db45e66a-cfff-4f33-8847-bbaedcbd32d6" // old openEuler ESP, gone
	stale2 := "49e639e8-0023-48ef-9990-f55882e78674" // old Rocky root, gone
	// lsblk returns the CURRENT partition table (post-write).
	exec.OnFull["lsblk -lnbo NAME,PARTUUID /dev/sda"] = mockExecResult{Out: []byte(
		"/dev/sda1 " + currentESP + "\n" +
			"/dev/sda2 " + currentRoot + "\n",
	)}
	exec.OnFull["efibootmgr -v"] = mockExecResult{Out: []byte(
		// Fresh entry — keep.
		"Boot0006* metalkit\tHD(1,GPT," + currentESP + ",0x2800,0x35000)/File(\\EFI\\metalkit\\shimx64.efi)\n" +
			// Stale from previous install — delete.
			"Boot0000* openEuler\tHD(1,GPT," + stale1 + ",0x800,0x3ff800)/File(\\EFI\\openEuler\\grubx64.efi)\n" +
			// Stale from older install — delete.
			"Boot0001* Rocky Linux\tHD(1,GPT," + stale2 + ",0x800,0x31800)/File(\\EFI\\rocky\\shimx64.efi)\n",
	)}
	exec.On["efibootmgr"] = mockExecResult{}
	rep := &mockReporter{}
	deps := Deps{Exec: exec, FS: newMockFS(), Reporter: rep}

	pruneStaleNVRAM(context.Background(), deps, "/dev/sda")

	var deletedEntries []string
	for _, c := range exec.Calls() {
		if c.Name == "efibootmgr" && len(c.Args) >= 3 && c.Args[0] == "-b" && c.Args[2] == "-B" {
			deletedEntries = append(deletedEntries, c.Args[1])
		}
	}
	if len(deletedEntries) != 2 {
		t.Fatalf("expected 2 efibootmgr deletions, got %d (%v)", len(deletedEntries), deletedEntries)
	}
	// Should have deleted Boot0000 and Boot0001 but NOT Boot0006 (current).
	want := map[string]bool{"0000": true, "0001": true}
	for _, e := range deletedEntries {
		if !want[e] {
			t.Fatalf("unexpected entry deletion: %s", e)
		}
	}
}

func TestPruneStaleNVRAM_DeletesMBREntries(t *testing.T) {
	exec := newMockExec()
	currentESP := "4469adb0-1041-490e-aab2-da41479c1d34"
	exec.OnFull["lsblk -lnbo NAME,PARTUUID /dev/sda"] = mockExecResult{Out: []byte(
		"/dev/sda1 " + currentESP + "\n",
	)}
	// Mix: a current GPT entry (keep), a stale MBR entry (delete — the
	// MBR boot sector was overwritten by the fresh image), and VenHw/BBS
	// entries (keep — we can't tell what they point at).
	exec.OnFull["efibootmgr -v"] = mockExecResult{Out: []byte(
		"Boot0006* metalkit\tHD(1,GPT," + currentESP + ",0x2800,0x35000)/File(\\EFI\\metalkit\\shimx64.efi)\n" +
			"Boot0008* openEuler\tHD(1,MBR,0x535913,0x800,0x3ff800)/File(\\EFI\\openEuler\\grubx64.efi)\n" +
			"Boot0001* Integrated NIC\tVenHw(3a191845-5f86-4e78-8fce-c4cff59f9daa)\n" +
			"Boot0002* IBA GE Slot\tBBS(128,IBA GE Slot 0100 v1578,0x0)\n",
	)}
	exec.On["efibootmgr"] = mockExecResult{}
	rep := &mockReporter{}
	deps := Deps{Exec: exec, FS: newMockFS(), Reporter: rep}

	pruneStaleNVRAM(context.Background(), deps, "/dev/sda")

	var deletedEntries []string
	for _, c := range exec.Calls() {
		if c.Name == "efibootmgr" && len(c.Args) >= 3 && c.Args[0] == "-b" && c.Args[2] == "-B" {
			deletedEntries = append(deletedEntries, c.Args[1])
		}
	}
	// Only Boot0008 (MBR) should be deleted. NIC (VenHw) and IBA (BBS)
	// are kept; Boot0006 is current.
	if len(deletedEntries) != 1 || deletedEntries[0] != "0008" {
		t.Fatalf("expected only Boot0008 deleted, got %v", deletedEntries)
	}
}

func TestPruneStaleNVRAM_KeepsNonHDEntries(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnbo NAME,PARTUUID /dev/sda"] = mockExecResult{Out: []byte("")}
	exec.OnFull["efibootmgr -v"] = mockExecResult{Out: []byte(
		"Boot0001* Integrated NIC\tVenHw(3a191845-5f86-4e78-8fce-c4cff59f9daa)\n" +
			"Boot0002* IBA GE Slot\tBBS(128,IBA GE Slot 0100 v1578,0x0)\n" +
			"Boot0003* EFI Fixed Disk\tPciRoot(0x0)/Pci(0x1,0x0)/Ctrl(0x1)/SCSI(4,0)/HD(15,GPT,dd1ff24b-...,0x2800,0x35000)\n",
	)}
	exec.On["efibootmgr"] = mockExecResult{}
	deps := Deps{Exec: exec, FS: newMockFS(), Reporter: &mockReporter{}}

	pruneStaleNVRAM(context.Background(), deps, "/dev/sda")

	// VenHw and BBS must never be touched. The PciRoot/HD entry references
	// a GUID not in current set — it WOULD be deleted, but only if the
	// regex matched; here the GUID is truncated to "dd1ff24b-..." so the
	// regex won't match and it's kept.
	for _, c := range exec.Calls() {
		if c.Name == "efibootmgr" && len(c.Args) >= 3 && c.Args[0] == "-b" {
			t.Fatalf("no deletions expected; call=%v", c)
		}
	}
}

func TestPruneStaleNVRAM_LsblkFailureStillRuns(t *testing.T) {
	// If we can't enumerate current partitions, we should still delete
	// MBR entries (they're always stale post-write) but leave GPT
	// entries alone (can't tell if they're current).
	exec := newMockExec()
	exec.OnFull["lsblk -lnbo NAME,PARTUUID /dev/sda"] = mockExecResult{Err: errString("no disk")}
	exec.OnFull["efibootmgr -v"] = mockExecResult{Out: []byte(
		"Boot0008* openEuler\tHD(1,MBR,0x535913,0x800,0x3ff800)/File(\\EFI\\openEuler\\grubx64.efi)\n" +
			"Boot0006* metalkit\tHD(1,GPT,4469adb0-1041-490e-aab2-da41479c1d34,0x2800,0x35000)/File(\\EFI\\metalkit\\shimx64.efi)\n",
	)}
	exec.On["efibootmgr"] = mockExecResult{}
	deps := Deps{Exec: exec, FS: newMockFS(), Reporter: &mockReporter{}}

	pruneStaleNVRAM(context.Background(), deps, "/dev/sda")

	var deletedEntries []string
	for _, c := range exec.Calls() {
		if c.Name == "efibootmgr" && len(c.Args) >= 3 && c.Args[0] == "-b" && c.Args[2] == "-B" {
			deletedEntries = append(deletedEntries, c.Args[1])
		}
	}
	// Only the MBR entry is deletable without partition info.
	if len(deletedEntries) != 1 || deletedEntries[0] != "0008" {
		t.Fatalf("expected only Boot0008 (MBR) deleted, got %v", deletedEntries)
	}
}

func TestPruneStaleNVRAM_EfibootmgrFailureNoOps(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnbo NAME,PARTUUID /dev/sda"] = mockExecResult{Out: []byte("")}
	exec.OnFull["efibootmgr -v"] = mockExecResult{Err: errString("no efivars")}
	deps := Deps{Exec: exec, FS: newMockFS(), Reporter: &mockReporter{}}

	// Must not panic or error.
	pruneStaleNVRAM(context.Background(), deps, "/dev/sda")

	// No -b -B deletion calls should have happened.
	for _, c := range exec.Calls() {
		if c.Name == "efibootmgr" && len(c.Args) >= 3 && c.Args[0] == "-b" {
			t.Fatalf("efibootmgr -b -B should not be called when -v fails; call=%v", c)
		}
	}
}

func TestIsHexBootNum(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"0000", true},
		{"FFFF", true},
		{"ABCD", true},
		{"00FF", true},
		{"00ff", false}, // lowercase rejected — efibootmgr prints uppercase
		{"000", false},  // too short
		{"00000", false},
		{"GGGG", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isHexBootNum(c.in); got != c.want {
			t.Errorf("isHexBootNum(%q)=%v want %v", c.in, got, c.want)
		}
	}
}
