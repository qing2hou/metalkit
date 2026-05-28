package installer

import (
	"context"
	"strings"
	"testing"
)

func TestCreateESPIfMissing_AlreadyExists(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte("/dev/sda1 part\n/dev/sda2 part\n")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda1"] = mockExecResult{Out: []byte("vfat")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	espDev, err := createESPIfMissing(context.Background(), deps, "/dev/sda", "/dev/sda2", "uefi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if espDev != "/dev/sda1" {
		t.Fatalf("expected /dev/sda1, got %q", espDev)
	}
	// sgdisk must not have been called.
	for _, c := range exec.Calls() {
		if c.Name == "sgdisk" {
			t.Fatal("sgdisk must not be called when ESP exists")
		}
	}
}

func TestCreateESPIfMissing_BIOS_Skips(t *testing.T) {
	exec := newMockExec()
	deps := Deps{Exec: exec, FS: newMockFS()}

	espDev, err := createESPIfMissing(context.Background(), deps, "/dev/sda", "/dev/sda2", "bios")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if espDev != "" {
		t.Fatalf("expected empty esp dev for BIOS, got %q", espDev)
	}
}

func TestCreateESPIfMissing_UEFI_CreatesESP(t *testing.T) {
	exec := newMockExec()
	// No vfat partition — findESP returns empty.
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte("/dev/sda1 part\n/dev/sda2 part\n")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda1"] = mockExecResult{Out: []byte("ext4")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda2"] = mockExecResult{Out: []byte("ext4")}
	// sgdisk -p shows current partitions.
	exec.OnFull["sgdisk -p /dev/sda"] = mockExecResult{Out: []byte(
		"Number  Start (sector)    End (sector)  Size       Code  Name\n" +
			"   1            2048          526335   256.0 MiB  8300  Linux filesystem\n" +
			"   2          526336        41943006   19.8 GiB    8300  Linux filesystem\n",
	)}
	deps := Deps{Exec: exec, FS: newMockFS()}

	espDev, err := createESPIfMissing(context.Background(), deps, "/dev/sda", "/dev/sda2", "uefi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if espDev != "/dev/sda3" {
		t.Fatalf("expected /dev/sda3, got %q", espDev)
	}

	// Verify sgdisk -n and -t were called.
	var sawNew, sawType, sawMkfs bool
	for _, c := range exec.Calls() {
		switch c.Name {
		case "sgdisk":
			full := strings.Join(c.Args, " ")
			if strings.Contains(full, "-n") && strings.Contains(full, "3:0:+512M") {
				sawNew = true
			}
			if strings.Contains(full, "-t") && strings.Contains(full, "3:ef00") {
				sawType = true
			}
		case "mkfs.fat":
			if len(c.Args) >= 2 && c.Args[0] == "-F32" && c.Args[1] == "/dev/sda3" {
				sawMkfs = true
			}
		}
	}
	if !sawNew {
		t.Fatal("sgdisk -n not called for ESP creation")
	}
	if !sawType {
		t.Fatal("sgdisk -t not called for ESP type")
	}
	if !sawMkfs {
		t.Fatal("mkfs.fat not called for ESP format")
	}
}

func TestNextPartNum(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 1},
		{"Number  Start\n   1  2048\n   2  526336\n", 3},
		{"   5  1234  5678\n", 6},
		{"   1  0  0\n   3  0  0\n", 4},
	}
	for _, tc := range cases {
		if got := nextPartNum(tc.in); got != tc.want {
			t.Fatalf("nextPartNum(%q)=%d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestPartitionPath(t *testing.T) {
	cases := []struct {
		disk string
		num  int
		want string
	}{
		{"/dev/sda", 3, "/dev/sda3"},
		{"/dev/nvme0n1", 3, "/dev/nvme0n1p3"},
		{"/dev/mmcblk0", 2, "/dev/mmcblk0p2"},
	}
	for _, tc := range cases {
		if got := partitionPath(tc.disk, tc.num); got != tc.want {
			t.Fatalf("partitionPath(%s, %d)=%q, want %q", tc.disk, tc.num, got, tc.want)
		}
	}
}

func TestCreateESPIfMissing_SgdiskFailure(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{Out: []byte("/dev/sda1 part\n/dev/sda2 part\n")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda1"] = mockExecResult{Out: []byte("ext4")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda2"] = mockExecResult{Out: []byte("ext4")}
	exec.OnFull["sgdisk -p /dev/sda"] = mockExecResult{Out: []byte("1\n2\n")}
	exec.OnFull["sgdisk -n 3:0:+512M -t 3:ef00 /dev/sda"] = mockExecResult{Err: errString("sgdisk: failed")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	_, err := createESPIfMissing(context.Background(), deps, "/dev/sda", "/dev/sda2", "uefi")
	if err == nil || !strings.Contains(err.Error(), "sgdisk new ESP") {
		t.Fatalf("expected sgdisk error, got %v", err)
	}
}
