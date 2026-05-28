package installer

import (
	"context"
	"strings"
	"testing"
)

const lsblkRootDisk = `/dev/sda    disk
/dev/sda1   part
/dev/sda2   part
/dev/sda3   part
`

func TestMount_HappyPath(t *testing.T) {
	exec := newMockExec()
	// lsblk on the parent disk
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte(lsblkRootDisk)}
	// blkid on /dev/sda1 -> vfat (the ESP); /dev/sda2 -> ext4 (skip, not vfat)
	exec.OnFull["blkid -o value -s TYPE /dev/sda1"] = mockExecResult{Out: []byte("vfat")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda2"] = mockExecResult{Out: []byte("ext4")}
	fs := newMockFS()
	deps := Deps{Exec: exec, FS: fs}

	espMount, cleanup, err := Mount(context.Background(), deps, "/dev/sda3", "/mnt/root")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if espMount != "/mnt/root/boot/efi" {
		t.Fatalf("esp mount path %s", espMount)
	}
	if cleanup == nil {
		t.Fatal("cleanup is nil")
	}
	// Verify mount ordering: root first, ESP, then binds.
	want := []string{"/mnt/root", "/mnt/root/boot/efi",
		"/mnt/root/proc", "/mnt/root/sys", "/mnt/root/dev",
		"/mnt/root/dev/pts", "/mnt/root/run"}
	var gotMounts []string
	for _, c := range exec.Calls() {
		if c.Name == "mount" && len(c.Args) >= 2 {
			// For binds the last arg is the target.
			gotMounts = append(gotMounts, c.Args[len(c.Args)-1])
		}
	}
	// Strip the efivars mount (best-effort, may be last).
	for i, m := range gotMounts {
		if strings.HasSuffix(m, "/sys/firmware/efi/efivars") {
			gotMounts = append(gotMounts[:i], gotMounts[i+1:]...)
			break
		}
	}
	if len(gotMounts) < len(want) {
		t.Fatalf("got %d mount targets, want %d: %v", len(gotMounts), len(want), gotMounts)
	}
	for i, w := range want {
		if gotMounts[i] != w {
			t.Fatalf("mount[%d]=%q want %q (all=%v)", i, gotMounts[i], w, gotMounts)
		}
	}

	// Cleanup unmounts in LIFO order.
	preCleanupCount := len(exec.Calls())
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup err: %v", err)
	}
	post := exec.Calls()[preCleanupCount:]
	if len(post) == 0 {
		t.Fatal("cleanup did not run any umount calls")
	}
	// First umount should be the LAST thing we mounted (efivars if it
	// succeeded, otherwise /run). Last umount should be /mnt/root.
	if post[len(post)-1].Args[len(post[len(post)-1].Args)-1] != "/mnt/root" {
		t.Fatalf("last umount must be /mnt/root, got %v", post[len(post)-1])
	}
}

func TestMount_NoESP_SkipsESP(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte(lsblkRootDisk)}
	// All non-vfat — no ESP on this disk.
	exec.OnFull["blkid -o value -s TYPE /dev/sda1"] = mockExecResult{Out: []byte("ext4")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda2"] = mockExecResult{Out: []byte("ext4")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	espMount, cleanup, err := Mount(context.Background(), deps, "/dev/sda3", "/mnt/root")
	if err != nil {
		t.Fatalf("no-ESP mount should succeed (BIOS image): %v", err)
	}
	if espMount != "" {
		t.Fatalf("esp mount path should be empty for BIOS, got %q", espMount)
	}
	if cleanup == nil {
		t.Fatal("cleanup is nil")
	}

	// Verify no ESP or efivars mounts.
	for _, c := range exec.Calls() {
		if c.Name != "mount" || len(c.Args) < 2 {
			continue
		}
		target := c.Args[len(c.Args)-1]
		if target == "/mnt/root/boot/efi" {
			t.Fatal("ESP must not be mounted when no ESP found")
		}
		if strings.HasSuffix(target, "/sys/firmware/efi/efivars") {
			t.Fatal("efivars must not be mounted on BIOS path")
		}
	}

	// Should still have root + bind mounts.
	var gotMounts []string
	for _, c := range exec.Calls() {
		if c.Name == "mount" && len(c.Args) >= 2 {
			gotMounts = append(gotMounts, c.Args[len(c.Args)-1])
		}
	}
	want := []string{"/mnt/root", "/mnt/root/proc", "/mnt/root/sys",
		"/mnt/root/dev", "/mnt/root/dev/pts", "/mnt/root/run"}
	if len(gotMounts) != len(want) {
		t.Fatalf("got %d mount targets, want %d: %v", len(gotMounts), len(want), gotMounts)
	}
	for i, w := range want {
		if gotMounts[i] != w {
			t.Fatalf("mount[%d]=%q want %q (all=%v)", i, gotMounts[i], w, gotMounts)
		}
	}
}

// Ubuntu 24.04 (Noble) cloud images carve out a separate /boot partition
// (LABEL=BOOT) plus the usual ESP (LABEL=UEFI). Without fstab-driven
// mounting, update-grub writes /boot/grub/grub.cfg into the root partition,
// which is masked at first real boot when LABEL=BOOT is mounted over /boot
// — the symptom is `grub rescue>` because grubx64.efi can't load grub.cfg.
// This test pins the fix: Mount() must mount /boot before /boot/efi, and
// must not fall back to findESP when fstab declares /boot/efi.
func TestMount_NobleSeparateBoot(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/etc/fstab"] = []byte(
		"LABEL=cloudimg-rootfs / ext4 discard,commit=30,errors=remount-ro 0 1\n" +
			"LABEL=BOOT /boot ext4 defaults 0 2\n" +
			"LABEL=UEFI /boot/efi vfat umask=0077 0 1\n",
	)
	exec.OnFull["blkid -L BOOT"] = mockExecResult{Out: []byte("/dev/sda16\n")}
	exec.OnFull["blkid -L UEFI"] = mockExecResult{Out: []byte("/dev/sda15\n")}
	deps := Deps{Exec: exec, FS: fs}

	espMount, cleanup, err := Mount(context.Background(), deps, "/dev/sda1", "/mnt/root")
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if espMount != "/mnt/root/boot/efi" {
		t.Fatalf("esp mount = %q, want /mnt/root/boot/efi", espMount)
	}
	defer cleanup()

	// Order must be: root, /boot (parent), /boot/efi (child), then binds.
	want := []string{
		"/mnt/root",
		"/mnt/root/boot",
		"/mnt/root/boot/efi",
		"/mnt/root/proc", "/mnt/root/sys", "/mnt/root/dev",
		"/mnt/root/dev/pts", "/mnt/root/run",
	}
	var got []string
	for _, c := range exec.Calls() {
		if c.Name != "mount" || len(c.Args) < 2 {
			continue
		}
		target := c.Args[len(c.Args)-1]
		if strings.HasSuffix(target, "/sys/firmware/efi/efivars") {
			continue
		}
		got = append(got, target)
	}
	if len(got) < len(want) {
		t.Fatalf("got %d mounts want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("mount[%d]=%q want %q (all=%v)", i, got[i], w, got)
		}
	}

	// findESP must NOT have been consulted; fstab is authoritative.
	for _, c := range exec.Calls() {
		if c.Name == "lsblk" {
			t.Fatalf("findESP was called; fstab should win: %v", c)
		}
	}

	// /boot must be mounted with the device blkid resolved (not LABEL=).
	for _, c := range exec.Calls() {
		if c.Name == "mount" && len(c.Args) >= 2 && c.Args[len(c.Args)-1] == "/mnt/root/boot" {
			if c.Args[0] != "/dev/sda16" {
				t.Fatalf("/boot mounted from %q want /dev/sda16", c.Args[0])
			}
		}
	}
}

// Cloud images that don't ship an /etc/fstab (e.g. an old CentOS image we
// support) must still get their ESP via the legacy findESP scan. This pins
// the fallback path so we don't accidentally make fstab mandatory.
func TestMount_NoFstabFallsBackToFindESP(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte(lsblkRootDisk)}
	exec.OnFull["blkid -o value -s TYPE /dev/sda1"] = mockExecResult{Out: []byte("vfat")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda2"] = mockExecResult{Out: []byte("ext4")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	espMount, cleanup, err := Mount(context.Background(), deps, "/dev/sda3", "/mnt/root")
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer cleanup()
	if espMount != "/mnt/root/boot/efi" {
		t.Fatalf("esp mount = %q want /mnt/root/boot/efi", espMount)
	}
}

func TestParentDiskOf(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/dev/sda3", "/dev/sda"},
		{"/dev/sda", "/dev/sda"},
		{"/dev/nvme0n1p3", "/dev/nvme0n1"},
		{"/dev/nvme0n1p10", "/dev/nvme0n1"},
		{"/dev/mmcblk0p1", "/dev/mmcblk0"},
	}
	for _, tc := range cases {
		if got := parentDiskOf(tc.in); got != tc.want {
			t.Fatalf("parentDiskOf(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}
