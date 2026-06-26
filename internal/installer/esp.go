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
		// ESP already exists — but its partition type code may be wrong.
		// Some cloud images (openEuler 24.03) ship the ESP with MBR type
		// 0x0E (W95 FAT16 LBA) instead of 0xEF, which strict UEFI firmware
		// (Dell R630) refuses to treat as an ESP. Fix it before returning.
		if bootMode == "uefi" {
			if ferr := fixESPTypeIfWrong(ctx, deps, parent, espDev); ferr != nil && deps.Logger != nil {
				deps.Logger.Warn("ESP partition type fix failed (continuing)", "err", ferr)
			}
		}
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

// fixESPTypeIfWrong ensures the ESP partition has the correct partition
// type code for UEFI boot. MBR disks need type 0xEF; GPT disks need EF00.
//
// Some cloud images ship the ESP with the wrong type code:
//   - openEuler 24.03 qcow2 uses MBR with ESP type 0x0E (W95 FAT16 LBA)
//     instead of 0xEF. Dell R630 firmware (and other strict UEFI impls)
//     refuses to treat 0x0E as an ESP — it reports "Boot Failed" even
//     though the EFI files are present and the NVRAM entry points at the
//     right partition.
//
// This is idempotent: if the type is already correct, it's a no-op. The
// fix uses sfdisk --part-type which works on both MBR and GPT disks.
// partprobe is called afterward so the kernel re-reads the table; if the
// ESP is already mounted the re-read may fail with EBUSY, but the change
// persists on disk and takes effect on next boot.
func fixESPTypeIfWrong(ctx context.Context, deps Deps, diskDev, espDev string) error {
	n, err := PartitionNumber(diskDev, espDev)
	if err != nil {
		return fmt.Errorf("install: fix ESP type: %w", err)
	}
	partNum := strconv.Itoa(n)

	// Detect partition table type from sfdisk -d output.
	out, err := deps.Exec.Run(ctx, "sfdisk", "-d", diskDev)
	if err != nil {
		return fmt.Errorf("install: sfdisk -d %s: %w", diskDev, err)
	}
	tableType := detectPartitionTableType(string(out))
	if tableType == "" {
		return fmt.Errorf("install: fix ESP type: could not determine partition table type for %s", diskDev)
	}

	// GPT disks written from cloud images carry the source disk's size in
	// the PMBR/backup header. After dd-ing onto a larger disk, sfdisk
	// refuses any write — "GPT PMBR size mismatch" / "backup GPT table is
	// not on the end of the device" — which blocks the part-type change
	// below. sgdisk -e relocates the backup GPT to the actual end and
	// rewrites the PMBR to match; partprobe then reloads the kernel view.
	// Best-effort: log failure but keep going, since sfdisk may succeed on
	// disks where the headers already match.
	if tableType == "gpt" {
		if _, eerr := deps.Exec.Run(ctx, "sgdisk", "-e", diskDev); eerr != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("fix ESP type: sgdisk -e failed (continuing)", "err", eerr)
			}
		}
		_, _ = deps.Exec.Run(ctx, "partprobe", diskDev)
	}

	// Read current partition type.
	typeOut, err := deps.Exec.Run(ctx, "sfdisk", "--part-type", diskDev, partNum)
	if err != nil {
		return fmt.Errorf("install: sfdisk --part-type %s %s: %w", diskDev, partNum, err)
	}
	current := strings.TrimSpace(string(typeOut))

	var want string
	switch tableType {
	case "dos":
		// MBR: ESP must be 0xEF. Normalise to lowercase to match sfdisk output.
		want = "ef"
	case "gpt":
		// GPT: ESP must be EF00. sfdisk prints this verbatim.
		want = "EF00"
	default:
		return nil
	}

	if strings.EqualFold(current, want) {
		return nil
	}

	if deps.Reporter != nil {
		_ = deps.Reporter.Log(ctx, "info",
			fmt.Sprintf("fixing ESP partition type on %s%s: %s -> %s (%s table)",
				diskDev, partNum, current, want, tableType))
	}
	if _, err := deps.Exec.Run(ctx, "sfdisk", "--part-type", diskDev, partNum, want); err != nil {
		// sfdisk sometimes still refuses even after sgdisk -e (e.g. when
		// the PMBR itself is structurally broken). Fall back to sgdisk
		// -t which is more permissive about header inconsistencies.
		if deps.Logger != nil {
			deps.Logger.Warn("fix ESP type: sfdisk --part-type failed, falling back to sgdisk -t",
				"err", err)
		}
		sgdiskType := fmt.Sprintf("%d:ef00", n)
		if out, serr := deps.Exec.Run(ctx, "sgdisk", "-t", sgdiskType, diskDev); serr != nil {
			return fmt.Errorf("install: sfdisk --part-type %s %s %s: %w (sgdisk fallback also failed: %v, output: %s)",
				diskDev, partNum, want, err, serr, string(out))
		}
	}
	// Trigger kernel re-read. Best-effort: EBUSY is expected when the
	// partition is mounted; the change still persists on disk.
	_, _ = deps.Exec.Run(ctx, "partprobe", diskDev)
	return nil
}

// detectPartitionTableType parses sfdisk -d output for the "label:" line.
// Returns "dos" (MBR), "gpt", or "" if not found.
func detectPartitionTableType(sfdiskDump string) string {
	for _, line := range strings.Split(sfdiskDump, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "label:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "label:"))
		}
	}
	return ""
}
