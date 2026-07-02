// grow.go expands the rootfs partition to fill the target disk, then
// resizes its filesystem.
//
// Identifying the rootfs: iterate partitions in REVERSE numeric order and
// pick the first one whose blkid TYPE is a real Linux filesystem
// (ext2/3/4, xfs). We can NOT just take "the highest-numbered partition"
// because Ubuntu cloud images put the ESP at the tail:
//
//	/dev/sda1   ext4   rootfs
//	/dev/sda14         BIOS boot stub (no FS)
//	/dev/sda15  vfat   ESP
//
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
		// e2fsck exit codes: 0 = clean, 1 = errors corrected (OK),
		// 2 = corrected + reboot needed, 4+ = real errors. openEuler
		// cloud images ship without /lost+found, so e2fsck -fy creates
		// it and returns exit 1 — this is benign, not a failure.
		if out, err := deps.Exec.Run(ctx, "e2fsck", "-fy", partDev); err != nil {
			// Allow exit code 1 (FS modified / errors corrected).
			// The Exec interface doesn't expose the raw exit code,
			// but e2fsck prints "FILE SYSTEM WAS MODIFIED" on exit 1
			// while real errors include "UNEXPECTED INCONSISTENCY".
			combined := strings.ToUpper(string(out) + " " + err.Error())
			if !strings.Contains(combined, "FILE SYSTEM WAS MODIFIED") &&
				!strings.Contains(combined, "CLEAN") {
				return res, fmt.Errorf("install: e2fsck %s: %w", partDev, err)
			}
			if deps.Logger != nil {
				deps.Logger.Info("e2fsck: filesystem modified (exit 1), proceeding",
					"dev", partDev, "output", string(out))
			}
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
// reverse-iteration with FS filtering beats "last partition", AND why
// LABEL inspection beats reverse-iteration on Ubuntu 24.04+ images that
// carry a separate /boot partition (LABEL=BOOT) — its ext4 fs would
// otherwise outrank the real root partition (LABEL=cloudimg-rootfs)
// purely because BOOT is numbered higher (e.g. sdc16 vs sdc1).
func rootPartitionOf(ctx context.Context, deps Deps, devPath string) (string, string, error) {
	enumerate := func() []string {
		out, err := deps.Exec.Run(ctx, "lsblk", "-lnpo", "NAME,TYPE", devPath)
		if err != nil {
			return nil
		}
		var ps []string
		scanner := bufio.NewScanner(strings.NewReader(string(out)))
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) >= 2 && fields[1] == "part" {
				ps = append(ps, fields[0])
			}
		}
		return ps
	}

	parts := enumerate()
	if len(parts) == 0 {
		// The kernel may still be serving a stale partition table even
		// after partprobe/blockdev in WriteImage — particularly right
		// after qemu-img convert wrote a fresh image. Force a re-read
		// and retry once before giving up. Without this, Debian 12
		// cloud image installs fail at "no partitions on /dev/sdX".
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "warn",
				fmt.Sprintf("no partitions visible on %s, forcing partprobe + blockdev --rereadpt and retrying", devPath))
		}
		_, _ = deps.Exec.Run(ctx, "partprobe", devPath)
		_, _ = deps.Exec.Run(ctx, "blockdev", "--rereadpt", devPath)
		// Give udev a moment to settle and re-create device nodes.
		_, _ = deps.Exec.Run(ctx, "udevadm", "settle", "--timeout=5")
		parts = enumerate()
	}
	if len(parts) == 0 {
		return "", "", fmt.Errorf("install: no partitions on %s", devPath)
	}

	// Pass 1: collect every partition whose blkid TYPE is a real Linux fs.
	type cand struct {
		dev   string
		fs    string
		label string
	}
	var cands []cand
	for _, p := range parts {
		typeOut, err := deps.Exec.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", p)
		if err != nil {
			continue
		}
		fs := strings.TrimSpace(string(typeOut))
		switch fs {
		case "", "vfat", "swap", "iso9660", "linux_raid_member", "LVM2_member":
			continue
		}
		labelOut, _ := deps.Exec.Run(ctx, "blkid", "-o", "value", "-s", "LABEL", p)
		cands = append(cands, cand{
			dev:   p,
			fs:    fs,
			label: strings.TrimSpace(string(labelOut)),
		})
	}
	if len(cands) == 0 {
		return "", "", fmt.Errorf("install: no rootfs candidate partition on %s", devPath)
	}

	// Pass 2: prefer candidates whose LABEL clearly identifies them as
	// rootfs (cloudimg-rootfs, *-rootfs, root, *_root). Penalise labels
	// that clearly identify them as a non-root system partition (BOOT,
	// EFI, ESP). This is what disambiguates Noble's sdc1 from sdc16.
	for _, c := range cands {
		if isRootLabel(c.label) {
			return c.dev, c.fs, nil
		}
	}
	// No positive root label found. Fall back to the original reverse-
	// numeric scan, BUT skip candidates whose label screams "not root"
	// (BOOT/EFI/ESP). This keeps backward compat on Jammy/RHEL/CentOS
	// images that don't bother labelling root, while still avoiding the
	// Noble trap when the image happens to label BOOT but not root.
	for i := len(cands) - 1; i >= 0; i-- {
		if isAntiRootLabel(cands[i].label) {
			continue
		}
		return cands[i].dev, cands[i].fs, nil
	}
	// Every candidate had an anti-root label — last resort, just return
	// the highest-numbered one and let install fail loudly downstream
	// rather than picking the wrong partition silently.
	last := cands[len(cands)-1]
	return last.dev, last.fs, nil
}

// isRootLabel reports whether a blkid LABEL clearly identifies a
// partition as the rootfs of a Linux distro image. Match is
// case-insensitive and based on the labels real-world cloud images use.
func isRootLabel(label string) bool {
	l := strings.ToLower(label)
	switch l {
	case "cloudimg-rootfs", "rootfs", "root", "_root":
		return true
	}
	return strings.HasSuffix(l, "-rootfs") ||
		strings.HasSuffix(l, "_root") ||
		strings.HasPrefix(l, "cloudimg-")
}

// isAntiRootLabel reports whether a blkid LABEL identifies a partition
// that definitely is NOT the rootfs (a separate /boot, ESP, or EFI
// system partition labelled at image build time).
func isAntiRootLabel(label string) bool {
	switch strings.ToUpper(label) {
	case "BOOT", "EFI", "ESP", "UEFI":
		return true
	}
	return false
}

// PartitionNumber returns the partition number embedded in partDev given
// its parent disk. Handles both /dev/sdaN and /dev/nvme0n1pN style
// names. Returns an error if partDev doesn't start with disk's path.
//
// Examples:
//
//	PartitionNumber("/dev/sda",      "/dev/sda3")        -> 3, nil
//	PartitionNumber("/dev/nvme0n1",  "/dev/nvme0n1p3")   -> 3, nil
//	PartitionNumber("/dev/mmcblk0",  "/dev/mmcblk0p1")   -> 1, nil
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
