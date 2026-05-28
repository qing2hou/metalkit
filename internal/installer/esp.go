// esp.go creates a FAT32 EFI System Partition when the live system booted
// via UEFI but the image has no ESP (common on older MBR-only images like
// CentOS 7.9). The ESP is placed at the end of the disk *before* the grow
// step, so growpart naturally stops at the ESP boundary.
package installer

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// createESPIfMissing checks whether an ESP exists on the same disk as
// partDev. If not, and bootMode is "uefi", it creates a 512 MiB FAT32
// partition at the end of the disk. Returns the ESP device path, or ""
// when no ESP was needed (BIOS boot or ESP already present).
func createESPIfMissing(ctx context.Context, deps Deps, diskDev, partDev, bootMode string) (string, error) {
	parent := parentDiskOf(diskDev)
	espDev, err := findESP(ctx, deps, parent, partDev)
	if err != nil {
		return "", err
	}
	if espDev != "" {
		return espDev, nil
	}
	if bootMode != "uefi" {
		return "", nil
	}

	// Zap any stale GPT structures left over from a previous UEFI install.
	// A BIOS/MBR image written over a GPT disk leaves a backup GPT header at
	// the end of the disk that confuses sgdisk -p.  -z clears GPT data without
	// touching the MBR.  Ignore errors — a pure MBR disk has nothing to zap.
	_, _ = deps.Exec.Run(ctx, "sgdisk", "-z", diskDev)

	// Find the next partition number from current table.
	out, err := deps.Exec.Run(ctx, "sgdisk", "-p", diskDev)
	if err != nil {
		return "", fmt.Errorf("install: sgdisk -p %s: %w", diskDev, err)
	}
	next := nextPartNum(string(out))

	// If the disk is MBR (e.g. CentOS 7.9 cloud image), convert to GPT first.
	// sgdisk -g converts MBR to GPT in memory and writes the result; on an
	// already-GPT disk it succeeds without changes.  Ignore errors — we only
	// need this to succeed when the disk is actually MBR.
	_, _ = deps.Exec.Run(ctx, "sgdisk", "-g", diskDev)

	// Create the partition at the end of the disk.
	partSpec := fmt.Sprintf("%d:0:+512M", next)
	typeSpec := fmt.Sprintf("%d:ef00", next)
	if _, err := deps.Exec.Run(ctx, "sgdisk", "-n", partSpec, "-t", typeSpec, diskDev); err != nil {
		return "", fmt.Errorf("install: sgdisk new ESP: %w", err)
	}

	espDev = partitionPath(diskDev, next)

	// Force kernel to re-read the partition table so the new device node
	// appears before we try to format it.
	_, _ = deps.Exec.Run(ctx, "partprobe", diskDev)

	// Format FAT32.
	if _, err := deps.Exec.Run(ctx, "mkfs.fat", "-F32", espDev); err != nil {
		return "", fmt.Errorf("install: mkfs.fat %s: %w", espDev, err)
	}

	if deps.Logger != nil {
		deps.Logger.Info("created ESP for UEFI boot",
			"disk", diskDev, "esp", espDev)
	}
	return espDev, nil
}

// nextPartNum returns the next available partition number from sgdisk -p output.
func nextPartNum(out string) int {
	max := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err == nil && n > max {
			max = n
		}
	}
	if max == 0 {
		return 1
	}
	return max + 1
}

// partitionPath returns the device node for partition partNum on diskDev,
// handling the NVMe / MMC "p" separator convention.
func partitionPath(diskDev string, partNum int) string {
	if strings.HasPrefix(diskDev, "/dev/nvme") || strings.HasPrefix(diskDev, "/dev/mmcblk") {
		return fmt.Sprintf("%sp%d", diskDev, partNum)
	}
	return fmt.Sprintf("%s%d", diskDev, partNum)
}
