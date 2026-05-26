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

func TestMount_NoESPFails(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte(lsblkRootDisk)}
	// All non-vfat
	exec.OnFull["blkid -o value -s TYPE /dev/sda1"] = mockExecResult{Out: []byte("ext4")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda2"] = mockExecResult{Out: []byte("ext4")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	_, _, err := Mount(context.Background(), deps, "/dev/sda3", "/mnt/root")
	if err == nil || !strings.Contains(err.Error(), "no FAT32 ESP") {
		t.Fatalf("want no-ESP error, got %v", err)
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
