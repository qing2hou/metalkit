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

func TestFixESPTypeIfWrong_MBR_Fixes0EtoEF(t *testing.T) {
	exec := newMockExec()
	// sfdisk -d shows MBR (dos) label.
	exec.OnFull["sfdisk -d /dev/sda"] = mockExecResult{Out: []byte(
		"label: dos\nlabel-id: 0x0c6569e1\nunit: sectors\n\n" +
			"/dev/sda1 : start=2048, size=4192256, type=e\n" +
			"/dev/sda2 : start=4194304, size=1166802911, type=83\n",
	)}
	// Current partition type is 0x0E (wrong).
	exec.OnFull["sfdisk --part-type /dev/sda 1"] = mockExecResult{Out: []byte("e\n")}
	// sfdisk --part-type ... ef succeeds (returns empty).
	exec.On["sfdisk"] = mockExecResult{}
	rep := &mockReporter{}
	deps := Deps{Exec: exec, FS: newMockFS(), Reporter: rep}

	if err := fixESPTypeIfWrong(context.Background(), deps, "/dev/sda", "/dev/sda1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Verify sfdisk --part-type /dev/sda 1 ef was called.
	var sawFix bool
	for _, c := range exec.Calls() {
		if c.Name == "sfdisk" && len(c.Args) >= 4 &&
			c.Args[0] == "--part-type" && c.Args[1] == "/dev/sda" &&
			c.Args[2] == "1" && c.Args[3] == "ef" {
			sawFix = true
		}
	}
	if !sawFix {
		t.Fatalf("sfdisk --part-type /dev/sda 1 ef was not called; calls=%v", exec.Calls())
	}
	// Verify partprobe was called best-effort.
	var sawPartprobe bool
	for _, c := range exec.Calls() {
		if c.Name == "partprobe" {
			sawPartprobe = true
		}
	}
	if !sawPartprobe {
		t.Fatal("partprobe not called after type fix")
	}
}

func TestFixESPTypeIfWrong_GPT_AlreadyEF00_NoOp(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["sfdisk -d /dev/sda"] = mockExecResult{Out: []byte(
		"label: gpt\nunit: sectors\n\n" +
			"/dev/sda1 : start=2048, size=4192256, type=EF00\n" +
			"/dev/sda2 : start=4194304, size=1166802911, type=8300\n",
	)}
	// Current type is already EF00.
	exec.OnFull["sfdisk --part-type /dev/sda 1"] = mockExecResult{Out: []byte("EF00\n")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	if err := fixESPTypeIfWrong(context.Background(), deps, "/dev/sda", "/dev/sda1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// sfdisk --part-type ... EF00 (the set call) must NOT be invoked.
	for _, c := range exec.Calls() {
		if c.Name == "sfdisk" && len(c.Args) == 4 && c.Args[0] == "--part-type" {
			t.Fatalf("sfdisk set part-type should not be called when type is already correct; call=%v", c)
		}
	}
}

func TestFixESPTypeIfWrong_MBR_AlreadyEF_NoOp(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["sfdisk -d /dev/sda"] = mockExecResult{Out: []byte("label: dos\n\n/dev/sda1 : start=2048, size=4192256, type=ef\n")}
	exec.OnFull["sfdisk --part-type /dev/sda 1"] = mockExecResult{Out: []byte("ef\n")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	if err := fixESPTypeIfWrong(context.Background(), deps, "/dev/sda", "/dev/sda1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, c := range exec.Calls() {
		if c.Name == "sfdisk" && len(c.Args) == 4 && c.Args[0] == "--part-type" {
			t.Fatalf("sfdisk set part-type should not be called when type is already ef; call=%v", c)
		}
	}
}

func TestFixESPTypeIfWrong_NVMePartitionNumber(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["sfdisk -d /dev/nvme0n1"] = mockExecResult{Out: []byte("label: dos\n\n/dev/nvme0n1p1 : start=2048, size=4192256, type=e\n")}
	exec.OnFull["sfdisk --part-type /dev/nvme0n1 1"] = mockExecResult{Out: []byte("e\n")}
	exec.On["sfdisk"] = mockExecResult{}
	deps := Deps{Exec: exec, FS: newMockFS()}

	if err := fixESPTypeIfWrong(context.Background(), deps, "/dev/nvme0n1", "/dev/nvme0n1p1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Verify partition number "1" was extracted from /dev/nvme0n1p1.
	var sawFix bool
	for _, c := range exec.Calls() {
		if c.Name == "sfdisk" && len(c.Args) >= 4 &&
			c.Args[0] == "--part-type" && c.Args[1] == "/dev/nvme0n1" &&
			c.Args[2] == "1" && c.Args[3] == "ef" {
			sawFix = true
		}
	}
	if !sawFix {
		t.Fatalf("sfdisk --part-type /dev/nvme0n1 1 ef was not called; calls=%v", exec.Calls())
	}
}

func TestDetectPartitionTableType(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"label: dos\n", "dos"},
		{"label: gpt\n", "gpt"},
		{"label: dos\nlabel-id: 0x123\n", "dos"},
		{"no label here\n", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := detectPartitionTableType(c.in); got != c.want {
			t.Errorf("detectPartitionTableType(%q)=%q want %q", c.in, got, c.want)
		}
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
