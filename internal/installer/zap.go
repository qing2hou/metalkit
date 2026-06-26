// zap.go wipes stale partition tables and NVRAM boot entries from the
// target disk so a fresh install never inherits structures left over by
// a previous OS install.
//
// We hit this on the Dell R630 test bed: a disk that had cycled through
// BIOS+MBR Rocky, then UEFI+GPT openEuler, ended up with a hybrid
// MBR/GPT state where the backup GPT header at the end of the disk
// survived multiple image writes. The ESP filesystem also accumulated
// \EFI\rocky, \EFI\proxmox, \EFI\metalkit dirs from prior installs,
// each carrying its own grubx64.efi with an embedded prefix pointing
// at a different path. The Dell firmware tried every NVRAM entry in
// BootOrder before reaching the working one, and a stale grubx64.efi
// whose prefix didn't match the on-disk layout was loaded first — it
// failed to find grub.cfg and the firmware reported "Boot Failed" for
// every entry, finally falling through to PXE.
//
// Two defenses, both best-effort:
//
//  1. wipeDisk runs `sgdisk -Z` (zap-all) before image write: destroys
//     both primary and backup GPT tables AND zeroes the MBR boot sector.
//     The subsequent qemu-img convert writes onto a clean slate with no
//     inherited structures. Safe because the user already confirmed
//     install intent by submitting the job.
//
//  2. pruneStaleNVRAM runs after grub-install: enumerate efibootmgr
//     entries and delete BootXXXX entries whose HD() device path
//     references a PARTUUID that existed on the target disk pre-write
//     but is now gone. Entries pointing at other disks (multi-OS
//     machines) are kept; entries without an HD() clause (VenHw, BBS,
//     PciRoot paths) are also kept since we can't tell what they point
//     at.
package installer

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// wipeDisk zaps all partition table signatures on devPath (both GPT
// primary+backup and MBR boot sector). Best-effort: failures are logged
// via Reporter but do not abort the install — sgdisk may be absent on
// minimal live images, and the subsequent image write will still
// produce a bootable disk in the common case, just without the
// stale-structure cleanup.
func wipeDisk(ctx context.Context, deps Deps, devPath string) {
	if _, err := deps.Exec.Run(ctx, "sgdisk", "-Z", devPath); err != nil {
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "warn",
				fmt.Sprintf("sgdisk -Z %s failed (continuing): %v", devPath, err))
		}
		if deps.Logger != nil {
			deps.Logger.Warn("sgdisk -Z failed", "dev", devPath, "err", err)
		}
		return
	}
	if deps.Reporter != nil {
		_ = deps.Reporter.Log(ctx, "info",
			fmt.Sprintf("zapped partition table on %s", devPath))
	}
}

// collectPartUUIDs returns the PARTUUIDs of all partitions currently on
// devPath, captured BEFORE the image write so pruneStaleNVRAM can later
// identify NVRAM entries that referenced partitions the write destroyed.
// Returns nil on any error; the caller treats nil as "no pruning possible".
func collectPartUUIDs(ctx context.Context, deps Deps, devPath string) []string {
	out, err := deps.Exec.Run(ctx, "lsblk", "-lnbo", "NAME,PARTUUID", devPath)
	if err != nil {
		return nil
	}
	var uuids []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// fields[0]=/dev/sda1, fields[1]=PARTUUID
		uuids = append(uuids, strings.ToLower(fields[1]))
	}
	return uuids
}

// nvramGPTPartUUIDRegex extracts the partition GUID from an efibootmgr -v
// HD(N,GPT,<guid>,start,size) device path clause. efibootmgr prints the
// GPT partition GUID verbatim.
var nvramGPTPartUUIDRegex = regexp.MustCompile(`HD\(\d+,GPT,([0-9a-fA-F-]+),`)

// nvramMBRSigRegex extracts the 32-bit MBR disk signature from an
// efibootmgr -v HD(N,MBR,<sig>,start,size) device path clause. efibootmgr
// prints the signature as a hex integer (no 0x prefix). MBR signatures
// are written by the OS into the MBR boot sector and persist across
// reinstalls of the same image family, so an entry like
// Boot0008 openEuler HD(1,MBR,0x535913,...) is a leftover from a previous
// install of the same image — its MBR signature no longer matches the
// freshly written disk and the entry is dead.
var nvramMBRSigRegex = regexp.MustCompile(`HD\(\d+,MBR,0x([0-9a-fA-F]+),`)

// pruneStaleNVRAM deletes BootXXXX entries whose HD() device path no
// longer matches a partition on the target disk. The strategy is a
// whitelist built from the CURRENT partition table (captured post-write,
// post-grow): any HD(GPT,<guid>,...) entry whose guid is not in the
// current set is stale; any HD(MBR,0x<sig>,...) entry is treated as
// stale because the fresh image write replaced the MBR boot sector and
// the old signature is gone. Entries without an HD() clause (VenHw for
// NIC PXE, BBS for legacy devices, PciRoot) are kept — we can't tell
// what they point at and they may be legitimate (e.g. the PXE entry the
// firmware needs to fall back to).
//
// This runs AFTER grub-install / registerEFIBootEntryRHEL have created
// the fresh BootXXXX entry for the just-installed OS, so that entry's
// GUID is in the current set and is preserved.
//
// Best-effort: efibootmgr failures are logged but don't abort the
// install. Caller is responsible for only invoking this on UEFI boots.
func pruneStaleNVRAM(ctx context.Context, deps Deps, devPath string) {
	currentUUIDs := collectPartUUIDs(ctx, deps, devPath)
	// haveCurrent tracks whether we successfully enumerated the current
	// partition table. When lsblk fails (or returns nothing) we can't
	// tell which GPT entries are still live, so we skip GPT pruning
	// entirely (conservative) and only delete MBR entries, which are
	// unconditionally stale post-write.
	haveCurrent := len(currentUUIDs) > 0
	currentSet := make(map[string]bool, len(currentUUIDs))
	for _, u := range currentUUIDs {
		currentSet[u] = true
	}
	out, err := deps.Exec.Run(ctx, "efibootmgr", "-v")
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("efibootmgr -v read failed; skipping NVRAM prune", "err", err)
		}
		return
	}
	var deleted []string
	for _, line := range strings.Split(string(out), "\n") {
		// Lines look like:
		//   Boot0000* Rocky Linux	HD(1,GPT,ebdee067-...,...)/File(\EFI\rocky\shimx64.efi)
		//   Boot0008* openEuler	HD(1,MBR,0x535913,...)/File(\EFI\openEuler\grubx64.efi)
		//   Boot0004* Integrated NIC 1 Port 1	VenHw(3a191845-...)
		//   BootOrder 0008,0006,0000,...
		//   BootCurrent 0004
		// We want only the "BootXXXX" entries (4 hex digits after "Boot").
		if !strings.HasPrefix(line, "Boot") || len(line) < 8 {
			continue
		}
		hex := line[4:8]
		if !isHexBootNum(hex) {
			continue
		}
		// MBR entries: always stale after a fresh image write (the MBR
		// boot sector was overwritten, so the old 0x<sig> no longer
		// identifies this disk).
		if m := nvramMBRSigRegex.FindStringSubmatch(line); m != nil {
			if _, err := deps.Exec.Run(ctx, "efibootmgr", "-b", hex, "-B"); err != nil {
				if deps.Logger != nil {
					deps.Logger.Warn("efibootmgr delete failed", "entry", hex, "err", err)
				}
				continue
			}
			deleted = append(deleted, hex+"(MBR)")
			continue
		}
		// GPT entries: only prune when we have a reliable current set;
		// otherwise we can't tell current from stale and might delete the
		// entry we just created.
		if !haveCurrent {
			continue
		}
		m := nvramGPTPartUUIDRegex.FindStringSubmatch(line)
		if m == nil {
			// No HD() clause at all (VenHw, BBS, PciRoot) — keep.
			continue
		}
		if currentSet[strings.ToLower(m[1])] {
			continue
		}
		if _, err := deps.Exec.Run(ctx, "efibootmgr", "-b", hex, "-B"); err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("efibootmgr delete failed", "entry", hex, "err", err)
			}
			continue
		}
		deleted = append(deleted, hex+"(GPT)")
	}
	if deps.Reporter != nil && len(deleted) > 0 {
		_ = deps.Reporter.Log(ctx, "info",
			fmt.Sprintf("pruned %d stale NVRAM boot entries: %s",
				len(deleted), strings.Join(deleted, ",")))
	}
}

// isHexBootNum reports whether s is a 4-char uppercase hex string
// (Boot0000..BootFFFF). efibootmgr prints entry numbers in uppercase
// hex; BootOrder/BootCurrent don't match this shape.
func isHexBootNum(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
