// grow.go expands the rootfs partition to fill the target disk, then
// resizes its filesystem.
//
// Identifying the rootfs: iterate partitions in REVERSE numeric order and
// pick the first one whose blkid TYPE is a real Linux filesystem
// (ext2/3/4, xfs). We can NOT just take "the highest-numbered partition"
// because Ubuntu cloud images put the ESP at the tail:
//   /dev/sda1   ext4   rootfs
//   /dev/sda14         BIOS boot stub (no FS)
//   /dev/sda15  vfat   ESP
// Reverse iteration with FS filtering walks past sda15/sda14 and lands on
// sda1, while Debian/Rocky/RHEL layouts (root is the highest-numbered)
// still match on the first probe.
//
// XFS can only be grown while mounted (xfs_growfs takes a mountpoint).
// To keep this file simple we handle ext2/3/4 inline and return a flag in
// the result so the orchestrator can run xfs_growfs after Mount.
package installer

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// GrowResult tells the caller what happened so the orchestrator can do
// the xfs-only post-mount step if needed.
type GrowResult struct {
	PartDev        string // /dev/sda3 or /dev/nvme0n1p3
	PartNum        int
	FSType         string // ext4 / xfs / etc.
	XFSPendingGrow bool   // true when caller must xfs_growfs after Mount
}

// GrowLastPartition finds the last partition on devPath, grows it to fill
// the disk, and resizes the filesystem (ext* inline; xfs flagged for
// post-mount). Idempotent: a "NOCHANGE" from growpart is treated as
// success.
func GrowLastPartition(ctx context.Context, deps Deps, devPath string) (GrowResult, error) {
	var res GrowResult
	if devPath == "" {
		return res, fmt.Errorf("install: GrowLastPartition: devPath is empty")
	}

	partDev, fsType, err := rootPartitionOf(ctx, deps, devPath)
	if err != nil {
		return res, err
	}
	partNum, err := PartitionNumber(devPath, partDev)
	if err != nil {
		return res, err
	}
	res.PartDev = partDev
	res.PartNum = partNum
	res.FSType = fsType

	// growpart returns exit 1 with NOCHANGE on the stdout when the
	// partition is already at the disk end. We can't tell from the Exec
	// interface whether the non-zero exit carries NOCHANGE without
	// inspecting the output — so we capture the err's Error() string,
	// which our OSExec packs the combined output into.
	if out, err := deps.Exec.Run(ctx, "growpart", devPath, strconv.Itoa(partNum)); err != nil {
		combined := strings.ToUpper(string(out) + " " + err.Error())
		if !strings.Contains(combined, "NOCHANGE") {
			return res, fmt.Errorf("install: growpart %s %d: %w", devPath, partNum, err)
		}
		if deps.Logger != nil {
			deps.Logger.Info("growpart NOCHANGE", "dev", devPath, "part", partNum)
		}
	}

	// blkid was already done by rootPartitionOf; res.FSType is set.

	switch res.FSType {
	case "ext2", "ext3", "ext4":
		if _, err := deps.Exec.Run(ctx, "e2fsck", "-fy", partDev); err != nil {
			return res, fmt.Errorf("install: e2fsck %s: %w", partDev, err)
		}
		if _, err := deps.Exec.Run(ctx, "resize2fs", partDev); err != nil {
			return res, fmt.Errorf("install: resize2fs %s: %w", partDev, err)
		}
	case "xfs":
		// Deferred: the orchestrator runs xfs_growfs after Mount.
		res.XFSPendingGrow = true
	default:
		return res, fmt.Errorf("install: unsupported fs %q on %s", res.FSType, partDev)
	}
	return res, nil
}

// rootPartitionOf enumerates partitions on devPath via lsblk and probes
// each with blkid (in reverse numeric order) to find the rootfs candidate.
// Returns (partition device path, FS type). See the file header for why
// reverse-iteration with FS filtering beats "last partition".
func rootPartitionOf(ctx context.Context, deps Deps, devPath string) (string, string, error) {
	out, err := deps.Exec.Run(ctx, "lsblk", "-lnpo", "NAME,TYPE", devPath)
	if err != nil {
		return "", "", fmt.Errorf("install: lsblk %s: %w", devPath, err)
	}
	var parts []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[1] == "part" {
			parts = append(parts, fields[0])
		}
	}
	if len(parts) == 0 {
		return "", "", fmt.Errorf("install: no partitions on %s", devPath)
	}
	// Reverse iteration: prefer the highest-numbered partition with a
	// growable rootfs FS. Stubs (BIOS-boot) have no FS so blkid errors;
	// vfat/swap/iso9660 are non-rootfs and skipped.
	for i := len(parts) - 1; i >= 0; i-- {
		out, err := deps.Exec.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", parts[i])
		if err != nil {
			continue
		}
		fs := strings.TrimSpace(string(out))
		switch fs {
		case "", "vfat", "swap", "iso9660", "linux_raid_member", "LVM2_member":
			continue
		}
		return parts[i], fs, nil
	}
	return "", "", fmt.Errorf("install: no rootfs candidate partition on %s", devPath)
}

// PartitionNumber returns the partition number embedded in partDev given
// its parent disk. Handles both /dev/sdaN and /dev/nvme0n1pN style
// names. Returns an error if partDev doesn't start with disk's path.
//
// Examples:
//   PartitionNumber("/dev/sda",      "/dev/sda3")        -> 3, nil
//   PartitionNumber("/dev/nvme0n1",  "/dev/nvme0n1p3")   -> 3, nil
//   PartitionNumber("/dev/mmcblk0",  "/dev/mmcblk0p1")   -> 1, nil
func PartitionNumber(disk, partDev string) (int, error) {
	if !strings.HasPrefix(partDev, disk) {
		return 0, fmt.Errorf("install: partition %q does not belong to disk %q", partDev, disk)
	}
	suffix := strings.TrimPrefix(partDev, disk)
	// Strip an optional 'p' separator used by nvme/mmcblk.
	suffix = strings.TrimPrefix(suffix, "p")
	if suffix == "" {
		return 0, fmt.Errorf("install: partition %q has no numeric suffix after %q", partDev, disk)
	}
	n, err := strconv.Atoi(suffix)
	if err != nil {
		return 0, fmt.Errorf("install: partition number from %q (suffix %q): %w", partDev, suffix, err)
	}
	return n, nil
}
