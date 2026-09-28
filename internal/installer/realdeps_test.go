package installer

import (
	"context"
	"testing"
)

const lsblkJSON = `{
  "blockdevices": [
    {"name":"sda","path":"/dev/sda","size":300000000000,"rm":false,"ro":false,
     "tran":"sata","model":"ST300MP0005","wwn":"0x5000c5008d4b0dab"},
    {"name":"sdb","path":"/dev/sdb","size":599550590976,"rm":false,"ro":false,
     "tran":"sas","model":"PERC H330 Mini","wwn":"0x644a8420336d9c00"}
  ]
}`

func TestLsblkDiskLister_ByPathFromUdev(t *testing.T) {
	m := newMockExec()
	m.On["lsblk"] = mockExecResult{Out: []byte(lsblkJSON)}
	m.OnFull["udevadm info -q property -n /dev/sda"] = mockExecResult{
		Out: []byte("DEVPATH=/devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block/sda\n" +
			"DEVNAME=/dev/sda\n" +
			"DEVLINKS=/dev/disk/by-path/pci-0000:00:1f.2-ata-1 /dev/disk/by-id/wwn-0x5000c5008d4b0dab\n"),
	}
	m.OnFull["udevadm info -q property -n /dev/sdb"] = mockExecResult{
		Out: []byte("DEVNAME=/dev/sdb\nDEVLINKS=/dev/disk/by-id/wwn-0x644a8420336d9c00\n"),
	}
	var lister LsblkDiskLister
	lister.Exec = m
	disks, err := lister.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(disks) != 2 {
		t.Fatalf("want 2 disks, got %d", len(disks))
	}
	if disks[0].ByPath != "/dev/disk/by-path/pci-0000:00:1f.2-ata-1" {
		t.Fatalf("sda ByPath = %q, want the by-path devlink", disks[0].ByPath)
	}
	if disks[1].ByPath != "" {
		t.Fatalf("sdb has no by-path devlink; ByPath = %q, want empty", disks[1].ByPath)
	}
}

// udevadm missing / erroring must degrade to empty ByPath, never fail the
// listing — PickDisk's DevPath/Name fallbacks still identify the disk.
func TestLsblkDiskLister_UdevadmFailureDegrades(t *testing.T) {
	m := newMockExec()
	m.On["lsblk"] = mockExecResult{Out: []byte(lsblkJSON)}
	m.On["udevadm"] = mockExecResult{Out: []byte("command not found"), Err: errString("exit 127")}
	var lister LsblkDiskLister
	lister.Exec = m
	disks, err := lister.List(context.Background())
	if err != nil {
		t.Fatalf("lister must not fail on udevadm error: %v", err)
	}
	for _, d := range disks {
		if d.ByPath != "" {
			t.Fatalf("disk %s ByPath = %q, want empty on udevadm failure", d.Name, d.ByPath)
		}
	}
}
