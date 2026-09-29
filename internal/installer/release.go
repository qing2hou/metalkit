// release.go drops kernel-level holders on the target disk before the image
// write so the partition table can actually be re-read afterwards.
//
// The failure this fixes (Dell R630, 2026-09-16): after streaming the image
// and calling partprobe, the kernel answered
//
//	Partition(s) 1, 3 on /dev/sda have been written, but we have been unable
//	to inform the kernel of the change, probably because it/they are in use
//	... ioctl error on BLKRRPART: Device or resource busy
//
// and kept serving the OLD partition table, so the freshly written image's
// partitions never appeared and the install died with "no rootfs candidate
// partition on /dev/sda". The holders came from the machine's previous OS
// (openEuler/Rocky layouts leave /boot plus an LVM PV or swap on the disk):
// the live image ships lvm2/mdadm/cryptsetup because some images need them,
// and systemd/udev auto-activate whatever exists on the host's disks at boot.
//
// Everything here is best-effort and logged: a failed release must not abort
// an install that would otherwise work, and the post-write re-read is the
// real arbiter of success.
package installer

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// lsblkRow is one parsed line of `lsblk -lnrpo NAME,TYPE,MOUNTPOINT <dev>`.
type lsblkRow struct {
	Name  string // full device path, e.g. /dev/sda3 or /dev/mapper/vg-root
	Type  string // disk | part | lvm | crypt | raid0.. | swap
	Mount string // mountpoint, empty when not mounted
}

// lsblkRows enumerates devPath and everything stacked on it, deepest child
// first (thanks to -r).
func lsblkRows(ctx context.Context, deps Deps, devPath string) []lsblkRow {
	out, err := deps.Exec.Run(ctx, "lsblk", "-lnrpo", "NAME,TYPE,MOUNTPOINT", devPath)
	if err != nil {
		return nil
	}
	var rows []lsblkRow
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		r := lsblkRow{Name: fields[0], Type: fields[1]}
		if len(fields) >= 3 {
			r.Mount = fields[2]
		}
		rows = append(rows, r)
	}
	return rows
}

// activeSwapsOn returns swap devices that sit on devPath (parsed from
// /proc/swaps). Empty on any error — the caller degrades to "nothing to do".
func activeSwapsOn(ctx context.Context, deps Deps, devPath string) []string {
	out, err := deps.Exec.Run(ctx, "cat", "/proc/swaps")
	if err != nil {
		return nil
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) < 2 {
		return nil
	}
	var devs []string
	for _, line := range lines[1:] { // skip the "Filename Type Size Used Priority" header
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(fields[0], devPath) {
			devs = append(devs, fields[0])
		}
	}
	return devs
}

// vgsOn returns the names of LVM VGs that have a physical volume on devPath.
// Other disks' VGs are deliberately left alone.
func vgsOn(ctx context.Context, deps Deps, devPath string) []string {
	out, err := deps.Exec.Run(ctx, "pvs", "--noheadings", "-o", "pv_name,vg_name")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var vgs []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if strings.HasPrefix(f[0], devPath) && f[1] != "" && !seen[f[1]] {
			seen[f[1]] = true
			vgs = append(vgs, f[1])
		}
	}
	return vgs
}

// releaseDisk unmounts, swaps off and deactivates everything holding devPath's
// partitions. Returns a human-readable list of the actions that succeeded so
// the caller can surface them ("what was holding the disk?").
func releaseDisk(ctx context.Context, deps Deps, devPath string) []string {
	var released []string

	rows := lsblkRows(ctx, deps, devPath)

	// 1. Unmounts (deepest first; -R covers nested mounts, -l survives a
	//    still-referenced fs — we are about to overwrite it anyway).
	for _, r := range rows {
		if r.Mount == "" {
			continue
		}
		if _, err := deps.Exec.Run(ctx, "umount", "-R", "-l", r.Name); err == nil {
			released = append(released, "umount "+r.Name)
		} else if deps.Logger != nil {
			deps.Logger.Warn("release: umount failed", "dev", r.Name, "err", err)
		}
	}

	// 2. Swap.
	for _, dev := range activeSwapsOn(ctx, deps, devPath) {
		if _, err := deps.Exec.Run(ctx, "swapoff", dev); err == nil {
			released = append(released, "swapoff "+dev)
		} else if deps.Logger != nil {
			deps.Logger.Warn("release: swapoff failed", "dev", dev, "err", err)
		}
	}

	// 3. LVM: vgchange -an is the proper deactivation (removing the dm
	//    devices alone leaves the VG marked active and udev re-creates them).
	for _, vg := range vgsOn(ctx, deps, devPath) {
		if _, err := deps.Exec.Run(ctx, "vgchange", "-an", vg); err == nil {
			released = append(released, "vgchange -an "+vg)
		} else if deps.Logger != nil {
			deps.Logger.Warn("release: vgchange -an failed", "vg", vg, "err", err)
		}
	}

	// 4. md arrays with members on this disk, and dm-crypt mappings.
	for _, r := range rows {
		if strings.HasPrefix(r.Type, "raid") {
			if _, err := deps.Exec.Run(ctx, "mdadm", "--stop", r.Name); err == nil {
				released = append(released, "mdadm --stop "+r.Name)
			} else if deps.Logger != nil {
				deps.Logger.Warn("release: mdadm --stop failed", "dev", r.Name, "err", err)
			}
		}
		if r.Type == "crypt" {
			name := path.Base(r.Name)
			if _, err := deps.Exec.Run(ctx, "cryptsetup", "close", name); err == nil {
				released = append(released, "cryptsetup close "+name)
			} else if deps.Logger != nil {
				deps.Logger.Warn("release: cryptsetup close failed", "name", name, "err", err)
			}
		}
	}

	return released
}

// logRelease writes the release summary to the install log (and the local
// logger). Silent when nothing was held.
func logRelease(ctx context.Context, deps Deps, devPath, stage string) {
	released := releaseDisk(ctx, deps, devPath)
	if len(released) == 0 {
		return
	}
	msg := fmt.Sprintf("released target-disk holders on %s (%s): %s",
		devPath, stage, strings.Join(released, ", "))
	if deps.Reporter != nil {
		_ = deps.Reporter.Log(ctx, "info", msg)
	}
	if deps.Logger != nil {
		deps.Logger.Info("released target-disk holders", "dev", devPath, "stage", stage,
			"actions", strings.Join(released, ", "))
	}
}
