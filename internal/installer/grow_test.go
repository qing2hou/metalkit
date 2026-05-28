package installer

import (
	"context"
	"strings"
	"testing"
)

func TestPartitionNumber(t *testing.T) {
	cases := []struct {
		disk    string
		part    string
		want    int
		wantErr bool
	}{
		{"/dev/sda", "/dev/sda3", 3, false},
		{"/dev/sda", "/dev/sda1", 1, false},
		{"/dev/nvme0n1", "/dev/nvme0n1p3", 3, false},
		{"/dev/nvme0n1", "/dev/nvme0n1p1", 1, false},
		{"/dev/mmcblk0", "/dev/mmcblk0p2", 2, false},
		{"/dev/sda", "/dev/sdb1", 0, true},
		{"/dev/sda", "/dev/sda", 0, true},
		{"/dev/nvme0n1", "/dev/nvme0n1p", 0, true},
	}
	for _, tc := range cases {
		got, err := PartitionNumber(tc.disk, tc.part)
		if (err != nil) != tc.wantErr {
			t.Fatalf("PartitionNumber(%q,%q) err=%v wantErr=%v", tc.disk, tc.part, err, tc.wantErr)
		}
		if err == nil && got != tc.want {
			t.Fatalf("PartitionNumber(%q,%q)=%d want %d", tc.disk, tc.part, got, tc.want)
		}
	}
}

const lsblkSDA = `NAME       TYPE
/dev/sda   disk
/dev/sda1  part
/dev/sda2  part
/dev/sda3  part
`

const lsblkNVMe = `NAME              TYPE
/dev/nvme0n1      disk
/dev/nvme0n1p1    part
/dev/nvme0n1p2    part
/dev/nvme0n1p3    part
`

func TestGrowLastPartition_Ext4_HappyPath(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte(lsblkSDA)}
	exec.OnFull["blkid -o value -s TYPE /dev/sda3"] = mockExecResult{Out: []byte("ext4\n")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	res, err := GrowLastPartition(context.Background(), deps, "/dev/sda")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.PartDev != "/dev/sda3" || res.PartNum != 3 || res.FSType != "ext4" {
		t.Fatalf("unexpected GrowResult: %+v", res)
	}
	if res.XFSPendingGrow {
		t.Fatal("ext4 must not flag XFSPendingGrow")
	}

	// Verify the growth actions ran in the right order on sda3. We don't
	// pin the blkid call count: rootPartitionOf may probe LABEL on
	// candidates, which is an internal detail.
	calls := exec.Calls()
	var names []string
	for _, c := range calls {
		switch c.Name {
		case "growpart", "e2fsck", "resize2fs":
			names = append(names, c.Name)
		}
	}
	want := []string{"growpart", "e2fsck", "resize2fs"}
	if len(names) != len(want) {
		t.Fatalf("growth ops: got %v want %v (all calls=%+v)", names, want, calls)
	}
	for i, w := range want {
		if names[i] != w {
			t.Fatalf("growth op[%d]=%q want %q", i, names[i], w)
		}
	}
}

func TestGrowLastPartition_NVMePathBuild(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/nvme0n1"] = mockExecResult{Out: []byte(lsblkNVMe)}
	exec.OnFull["blkid -o value -s TYPE /dev/nvme0n1p3"] = mockExecResult{Out: []byte("ext4")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	res, err := GrowLastPartition(context.Background(), deps, "/dev/nvme0n1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.PartDev != "/dev/nvme0n1p3" || res.PartNum != 3 {
		t.Fatalf("nvme partition wrong: %+v", res)
	}
}

func TestGrowLastPartition_NOCHANGE_IsSuccess(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte(lsblkSDA)}
	exec.On["growpart"] = mockExecResult{
		Out: []byte("NOCHANGE: partition 3 is size 488392704. it cannot be grown\n"),
		Err: errString("exit status 1"),
	}
	exec.OnFull["blkid -o value -s TYPE /dev/sda3"] = mockExecResult{Out: []byte("ext4")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	res, err := GrowLastPartition(context.Background(), deps, "/dev/sda")
	if err != nil {
		t.Fatalf("NOCHANGE growpart should be success: %v", err)
	}
	if res.PartDev != "/dev/sda3" {
		t.Fatalf("partdev wrong: %s", res.PartDev)
	}
}

func TestGrowLastPartition_XFSPending(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte(lsblkSDA)}
	exec.OnFull["blkid -o value -s TYPE /dev/sda3"] = mockExecResult{Out: []byte("xfs")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	res, err := GrowLastPartition(context.Background(), deps, "/dev/sda")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.XFSPendingGrow {
		t.Fatal("xfs must set XFSPendingGrow")
	}
	if res.FSType != "xfs" {
		t.Fatalf("fstype: %s", res.FSType)
	}
	// e2fsck / resize2fs MUST NOT be called for xfs.
	for _, c := range exec.Calls() {
		if c.Name == "e2fsck" || c.Name == "resize2fs" {
			t.Fatalf("xfs path called %s, must not", c.Name)
		}
	}
}

func TestGrowLastPartition_UnsupportedFS(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte(lsblkSDA)}
	exec.OnFull["blkid -o value -s TYPE /dev/sda3"] = mockExecResult{Out: []byte("btrfs")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	_, err := GrowLastPartition(context.Background(), deps, "/dev/sda")
	if err == nil || !strings.Contains(err.Error(), "unsupported fs") {
		t.Fatalf("want unsupported-fs err, got %v", err)
	}
}

func TestGrowLastPartition_GrowpartHardFail(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte(lsblkSDA)}
	exec.OnFull["blkid -o value -s TYPE /dev/sda3"] = mockExecResult{Out: []byte("ext4")}
	exec.On["growpart"] = mockExecResult{Err: errString("kaboom")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	_, err := GrowLastPartition(context.Background(), deps, "/dev/sda")
	if err == nil {
		t.Fatal("hard growpart failure must error")
	}
}

func TestGrowLastPartition_NoPartitions(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte("/dev/sda disk\n")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	_, err := GrowLastPartition(context.Background(), deps, "/dev/sda")
	if err == nil || !strings.Contains(err.Error(), "no partitions") {
		t.Fatalf("want no-partitions error, got %v", err)
	}
}

// TestGrowLastPartition_UbuntuCloudimgLayout locks in the rootfs-vs-ESP
// disambiguation fix. Ubuntu cloud images lay out partitions as
// sda1=ext4 root, sda14=BIOS-boot (no FS), sda15=vfat ESP. The old
// "last partition" logic picked sda15 and exploded with
// `unsupported fs "vfat"`. The fix iterates in reverse and skips
// non-rootfs filesystems until it lands on sda1.
func TestGrowLastPartition_UbuntuCloudimgLayout(t *testing.T) {
	const lsblkUbuntu = `/dev/sda    disk
/dev/sda1   part
/dev/sda14  part
/dev/sda15  part
`
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte(lsblkUbuntu)}
	// sda15 = ESP (vfat) → skipped
	exec.OnFull["blkid -o value -s TYPE /dev/sda15"] = mockExecResult{Out: []byte("vfat")}
	// sda14 = BIOS-boot, no FS → blkid errors → skipped
	exec.OnFull["blkid -o value -s TYPE /dev/sda14"] = mockExecResult{Err: errString("no fs")}
	// sda1 = rootfs (ext4) → picked
	exec.OnFull["blkid -o value -s TYPE /dev/sda1"] = mockExecResult{Out: []byte("ext4")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	res, err := GrowLastPartition(context.Background(), deps, "/dev/sda")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.PartDev != "/dev/sda1" || res.PartNum != 1 || res.FSType != "ext4" {
		t.Fatalf("Ubuntu cloudimg layout: got %+v, want /dev/sda1/1/ext4", res)
	}
}

// TestGrowLastPartition_NobleSeparateBoot pins the LABEL-driven rootfs
// selector. Ubuntu 24.04 cloud images carry both an ext4 root (sda1,
// LABEL=cloudimg-rootfs) and a separate ext4 /boot (sda16, LABEL=BOOT).
// The naive "last ext4 wins" rule would grow sda16, masking root grub
// install and ending in `grub rescue>` at first real boot. The fix
// must select sda1.
func TestGrowLastPartition_NobleSeparateBoot(t *testing.T) {
	const lsblkNoble = `/dev/sda    disk
/dev/sda1   part
/dev/sda14  part
/dev/sda15  part
/dev/sda16  part
`
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte(lsblkNoble)}
	// FS types
	exec.OnFull["blkid -o value -s TYPE /dev/sda1"] = mockExecResult{Out: []byte("ext4")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda14"] = mockExecResult{Err: errString("no fs")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda15"] = mockExecResult{Out: []byte("vfat")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda16"] = mockExecResult{Out: []byte("ext4")}
	// LABELs — only ext4 candidates get probed.
	exec.OnFull["blkid -o value -s LABEL /dev/sda1"] = mockExecResult{Out: []byte("cloudimg-rootfs")}
	exec.OnFull["blkid -o value -s LABEL /dev/sda16"] = mockExecResult{Out: []byte("BOOT")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	res, err := GrowLastPartition(context.Background(), deps, "/dev/sda")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.PartDev != "/dev/sda1" || res.PartNum != 1 || res.FSType != "ext4" {
		t.Fatalf("Noble layout: got %+v, want sda1/1/ext4 (LABEL=cloudimg-rootfs)", res)
	}

	// Belt and braces: BOOT must NOT have been grown.
	for _, c := range exec.Calls() {
		if c.Name == "growpart" && len(c.Args) >= 1 {
			if strings.HasSuffix(strings.Join(c.Args, " "), "16") {
				t.Fatalf("growpart targeted partition 16 (BOOT), should be 1: %v", c)
			}
		}
		if (c.Name == "e2fsck" || c.Name == "resize2fs") &&
			len(c.Args) > 0 && c.Args[len(c.Args)-1] == "/dev/sda16" {
			t.Fatalf("%s ran against /dev/sda16 (BOOT), should be /dev/sda1: %v", c.Name, c)
		}
	}
}

// TestRootLabelHelpers documents which blkid LABEL values count as
// "rootfs" and which count as "definitely not rootfs". If a future
// distro shows up with a label not covered here and the resulting
// disambiguation breaks installs, the fix lives in these tables.
func TestRootLabelHelpers(t *testing.T) {
	root := []string{"cloudimg-rootfs", "rootfs", "root", "_root",
		"my-rootfs", "system_root", "cloudimg-noble"}
	for _, l := range root {
		if !isRootLabel(l) {
			t.Errorf("isRootLabel(%q) = false, want true", l)
		}
	}
	notRoot := []string{"", "BOOT", "EFI", "ESP", "UEFI", "data", "home"}
	for _, l := range notRoot {
		if isRootLabel(l) {
			t.Errorf("isRootLabel(%q) = true, want false", l)
		}
	}
	anti := []string{"BOOT", "boot", "EFI", "efi", "ESP", "UEFI"}
	for _, l := range anti {
		if !isAntiRootLabel(l) {
			t.Errorf("isAntiRootLabel(%q) = false, want true", l)
		}
	}
	notAnti := []string{"", "cloudimg-rootfs", "data", "system"}
	for _, l := range notAnti {
		if isAntiRootLabel(l) {
			t.Errorf("isAntiRootLabel(%q) = true, want false", l)
		}
	}
}
