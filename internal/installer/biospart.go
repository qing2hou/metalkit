// biospart.go ensures a legacy-BIOS install on a GPT disk has the BIOS Boot
// Partition GRUB needs for its core.img. MBR disks don't need one — GRUB
// embeds between the MBR and the first partition.
//
// The partition is created at the end of the disk (same position/sequence as
// the ESP path) so the subsequent growpart still stops at the last data
// partition. The requested extent is 1 MiB but sgdisk aligns starts to 1 MiB
// boundaries, so the resulting partition is ~2 MiB; that is fine. The type
// GUID is the one GRUB's grub-install looks for when deciding where to embed.
package installer

import (
	"context"
	"fmt"
	"strings"
)

// biosBootTypeGUID is the GPT partition-type GUID reserved for BIOS boot
// (sgdisk shorthand 21686148-6449-6E6F-744E-656564454649).
const biosBootTypeGUID = "21686148-6449-6E6F-744E-656564454649"

// createBIOSBootIfMissing creates the BIOS Boot Partition on GPT disks when
// bootMode is "bios". Returns the new partition device, or "" when nothing
// was needed (non-BIOS mode, MBR disk, or the partition already exists).
func createBIOSBootIfMissing(ctx context.Context, deps Deps, diskDev, bootMode string) (string, error) {
	if bootMode != "bios" {
		return "", nil
	}

	// Detect the table type BEFORE touching GPT headers: running sgdisk -e on
	// an MBR disk with stale GPT remnants can revive them and overwrite the
	// MBR table we just wrote. Only relocate the backup header on real GPT.
	out, err := deps.Exec.Run(ctx, "sfdisk", "-d", diskDev)
	if err != nil {
		return "", fmt.Errorf("install: sfdisk -d %s (bios boot partition): %w", diskDev, err)
	}
	table := detectPartitionTableType(string(out))
	if table != "gpt" {
		// MBR: GRUB uses the post-MBR gap; nothing to create. Unknown table
		// types are handled conservatively (skip rather than repartition an
		// unrecognised disk).
		return "", nil
	}
	// After a fresh dd onto a larger disk the GPT backup header still sits at
	// the source disk's end, which makes sfdisk/sgdisk refuse writes. Relocate
	// it, then re-read the authoritative layout.
	if _, eerr := deps.Exec.Run(ctx, "sgdisk", "-e", diskDev); eerr != nil && deps.Logger != nil {
		deps.Logger.Warn("sgdisk -e failed (continuing)", "disk", diskDev, "err", eerr)
	}
	out, err = deps.Exec.Run(ctx, "sfdisk", "-d", diskDev)
	if err != nil {
		return "", fmt.Errorf("install: sfdisk -d %s after sgdisk -e: %w", diskDev, err)
	}

	// Some images already ship a BIOS Boot Partition. sfdisk -d prints the
	// type GUID (case may vary), so compare case-insensitively.
	if strings.Contains(strings.ToLower(string(out)), strings.ToLower(biosBootTypeGUID)) {
		return "", nil
	}

	// Place the partition at the ABSOLUTE END of the disk. sgdisk's start=0
	// means "start of the largest free block", which sits directly after the
	// last partition — that would block growpart from expanding the root
	// filesystem. "-1M:0" pins it to the final MiB, leaving root free to grow
	// up to its start.
	pOut, err := deps.Exec.Run(ctx, "sgdisk", "-p", diskDev)
	if err != nil {
		return "", fmt.Errorf("install: sgdisk -p %s (bios boot partition): %w", diskDev, err)
	}
	next := nextPartNum(string(pOut))
	partSpec := fmt.Sprintf("%d:-1M:0", next)
	typeSpec := fmt.Sprintf("%d:%s", next, biosBootTypeGUID)
	if _, err := deps.Exec.Run(ctx, "sgdisk", "-n", partSpec, "-t", typeSpec, diskDev); err != nil {
		return "", fmt.Errorf("install: sgdisk new BIOS boot partition %s: %w", partSpec, err)
	}

	dev := partitionPath(diskDev, next)
	if _, err := deps.Exec.Run(ctx, "partprobe", diskDev); err != nil && deps.Logger != nil {
		deps.Logger.Warn("partprobe after BIOS boot partition failed (continuing)", "disk", diskDev, "err", err)
	}
	// The new partition may reuse space that held an old filesystem signature;
	// wipe it so rootPartitionOf/blkid can't mistake it for a root candidate.
	if _, err := deps.Exec.Run(ctx, "wipefs", "-a", dev); err != nil && deps.Logger != nil {
		deps.Logger.Warn("wipefs on BIOS boot partition failed (continuing)", "dev", dev, "err", err)
	}
	if deps.Logger != nil {
		deps.Logger.Info("created BIOS Boot Partition", "disk", diskDev, "part", dev)
	}
	return dev, nil
}
