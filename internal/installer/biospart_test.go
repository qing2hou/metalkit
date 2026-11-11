package installer

import (
	"context"
	"strings"
	"testing"
)

const gptDumpNoBios = `label: gpt
label-id: 12345678-1234-1234-1234-123456789abc
device: /dev/sda
unit: sectors
first-lba: 2048
last-lba: 209715166
sector-size: 512

/dev/sda1 : start=        2048, size=     2048000, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, uuid=11111111-1111-1111-1111-111111111111
/dev/sda2 : start=     2050048, size=   207000000, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, uuid=22222222-2222-2222-2222-222222222222
`

const gptDumpWithBios = `label: gpt
device: /dev/sda
unit: sectors

/dev/sda1 : start=2048, size=2048000, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B
/dev/sda2 : start=2050048, size=1000, type=21686148-6449-6E6F-744E-656564454649
/dev/sda3 : start=2051048, size=207000000, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4
`

const mbrDump = `label: dos
device: /dev/sda
unit: sectors

/dev/sda1 : start=2048, size=2048000, type=83
`

const sgdiskPrint2Parts = `Number  Start (sector)    End (sector)  Size       Code  Name
   1            2048         2050047   1000.0 MiB  EF00  EFI System
   2         2050048       209715166   99.0 GiB    8300  Linux filesystem
`

func TestCreateBIOSBootIfMissing_NoOpOutsideBIOS(t *testing.T) {
	exec := newMockExec()
	deps := Deps{Exec: exec}
	dev, err := createBIOSBootIfMissing(context.Background(), deps, "/dev/sda", "uefi")
	if err != nil || dev != "" {
		t.Fatalf("uefi: dev=%q err=%v, want empty/nil", dev, err)
	}
	if len(exec.Calls()) != 0 {
		t.Fatalf("uefi: expected no commands, got %v", exec.Calls())
	}
}

func TestCreateBIOSBootIfMissing_MBRSkips(t *testing.T) {
	exec := newMockExec()
	exec.On["sfdisk"] = mockExecResult{Out: []byte(mbrDump)}
	deps := Deps{Exec: exec}
	dev, err := createBIOSBootIfMissing(context.Background(), deps, "/dev/sda", "bios")
	if err != nil || dev != "" {
		t.Fatalf("mbr: dev=%q err=%v", dev, err)
	}
	for _, c := range exec.Calls() {
		if c.Name == "sgdisk" && len(c.Args) > 0 && c.Args[0] == "-n" {
			t.Fatalf("mbr: must not create a partition, calls=%v", exec.Calls())
		}
	}
}

func TestCreateBIOSBootIfMissing_GPTCreates(t *testing.T) {
	exec := newMockExec()
	exec.On["sfdisk"] = mockExecResult{Out: []byte(gptDumpNoBios)}
	exec.OnFull["sgdisk -p /dev/sda"] = mockExecResult{Out: []byte(sgdiskPrint2Parts)}
	deps := Deps{Exec: exec}

	dev, err := createBIOSBootIfMissing(context.Background(), deps, "/dev/sda", "bios")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if dev != "/dev/sda3" {
		t.Errorf("dev = %q, want /dev/sda3", dev)
	}
	var sawCreate, sawProbe bool
	for _, c := range exec.Calls() {
		joined := strings.Join(c.Args, " ")
		switch {
		case c.Name == "sgdisk" &&
			strings.Contains(joined, "-n 3:-1M:0") &&
			strings.Contains(joined, "-t 3:"+biosBootTypeGUID):
			sawCreate = true
		case c.Name == "partprobe":
			sawProbe = true
		}
	}
	if !sawCreate {
		t.Fatalf("missing sgdisk create/type call: %v", exec.Calls())
	}
	if !sawProbe {
		t.Fatalf("missing partprobe: %v", exec.Calls())
	}
}

func TestCreateBIOSBootIfMissing_ExistingPartitionSkips(t *testing.T) {
	exec := newMockExec()
	exec.On["sfdisk"] = mockExecResult{Out: []byte(gptDumpWithBios)}
	deps := Deps{Exec: exec}
	dev, err := createBIOSBootIfMissing(context.Background(), deps, "/dev/sda", "bios")
	if err != nil || dev != "" {
		t.Fatalf("existing: dev=%q err=%v", dev, err)
	}
	for _, c := range exec.Calls() {
		if c.Name == "sgdisk" && len(c.Args) > 0 && c.Args[0] == "-n" {
			t.Fatalf("must not create a second BIOS boot partition: %v", exec.Calls())
		}
	}
}
