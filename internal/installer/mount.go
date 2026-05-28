// mount.go assembles the chroot environment for the post-write steps
// (seed ISO copy, grub-install, update-grub). It mounts the just-grown
// root partition, walks the target's /etc/fstab to mount any non-root
// real filesystems (e.g. a separate /boot — Ubuntu 24.04 cloud images
// have one — or /boot/efi when the image declares it that way), then
// bind-mounts the host's /proc /sys /dev /dev/pts /run inside, and
// finally efivars (best-effort).
//
// All mounts are torn down in LIFO order by the returned cleanup. The
// cleanup is intentionally lossy: a second-pass umount failure (e.g. busy
// after grub-install left a leftover) is logged but doesn't poison the
// install — we still want to report success if the disk is bootable.
package installer

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Mount mounts partDev (the freshly resized root) at mntRoot, finds the
// ESP on the same parent disk (first FAT/vfat partition) and mounts it at
// <mntRoot>/boot/efi, then bind-mounts the runtime pseudo-fs. Returns the
// mounted ESP path and a cleanup func that umounts in reverse order.
//
// On any failure during setup, cleanup is invoked internally and a
// no-op cleanup is returned so callers can safely defer it.
func Mount(ctx context.Context, deps Deps, partDev, mntRoot string) (string, func() error, error) {
	noop := func() error { return nil }
	if partDev == "" || mntRoot == "" {
		return "", noop, fmt.Errorf("install: Mount: partDev and mntRoot are required")
	}

	if err := deps.FS.MkdirAll(mntRoot, 0o755); err != nil {
		return "", noop, fmt.Errorf("install: mkdir %s: %w", mntRoot, err)
	}

	// Track mounts in LIFO order for cleanup.
	var mounted []string
	rollback := func() {
		for i := len(mounted) - 1; i >= 0; i-- {
			if _, err := deps.Exec.Run(ctx, "umount", mounted[i]); err != nil {
				if deps.Logger != nil {
					deps.Logger.Warn("umount during rollback failed",
						"mount", mounted[i], "err", err)
				}
			}
		}
	}

	if _, err := deps.Exec.Run(ctx, "mount", partDev, mntRoot); err != nil {
		return "", noop, fmt.Errorf("install: mount %s -> %s: %w", partDev, mntRoot, err)
	}
	mounted = append(mounted, mntRoot)

	// Walk the target image's /etc/fstab and mount its non-root, non-swap
	// real filesystems under mntRoot. Critical for Ubuntu 24.04 (Noble)
	// cloud images, which carve out a separate /boot partition (LABEL=BOOT)
	// — without this, update-grub writes /boot/grub/grub.cfg into the root
	// partition's /boot directory, and at first real boot the kernel mounts
	// the BOOT partition over /boot, masking grub.cfg and dropping into
	// `grub rescue>`. fstab also typically declares /boot/efi here, which
	// makes ESP detection authoritative; we still keep findESP as a fallback
	// for older cloud images that don't list /boot/efi in fstab.
	fstabMounts, espFromFstab, err := mountFromFstab(ctx, deps, mntRoot)
	if err != nil {
		rollback()
		return "", noop, err
	}
	mounted = append(mounted, fstabMounts...)

	// Locate the ESP. fstab is authoritative when it declares /boot/efi; we
	// only fall back to scanning the parent disk for the first vfat
	// partition when fstab doesn't tell us. Parent disk derivation strips
	// trailing digits and the optional 'p' separator on partDev.
	espMount := espFromFstab
	if espMount == "" {
		parent := parentDiskOf(partDev)
		espDev, err := findESP(ctx, deps, parent, partDev)
		if err != nil {
			rollback()
			return "", noop, err
		}
		if espDev != "" {
			espMount = filepath.Join(mntRoot, "boot", "efi")
			if err := deps.FS.MkdirAll(espMount, 0o755); err != nil {
				rollback()
				return "", noop, fmt.Errorf("install: mkdir %s: %w", espMount, err)
			}
			if _, err := deps.Exec.Run(ctx, "mount", espDev, espMount); err != nil {
				rollback()
				return "", noop, fmt.Errorf("install: mount ESP %s -> %s: %w", espDev, espMount, err)
			}
			mounted = append(mounted, espMount)
		}
	}

	// Bind-mount the runtime pseudo-fs so chroot grub-install sees the
	// host kernel's view (efivars, /dev/disk/by-*, /run/lock for grub).
	for _, sub := range []string{"/proc", "/sys", "/dev", "/dev/pts", "/run"} {
		target := filepath.Join(mntRoot, sub)
		if err := deps.FS.MkdirAll(target, 0o755); err != nil {
			rollback()
			return "", noop, fmt.Errorf("install: mkdir bind %s: %w", target, err)
		}
		if _, err := deps.Exec.Run(ctx, "mount", "--bind", sub, target); err != nil {
			rollback()
			return "", noop, fmt.Errorf("install: bind %s -> %s: %w", sub, target, err)
		}
		mounted = append(mounted, target)
	}

	// efivars is best-effort and only relevant for UEFI installs.
	if espMount != "" {
		efivars := filepath.Join(mntRoot, "sys", "firmware", "efi", "efivars")
		if _, err := deps.Exec.Run(ctx, "mount", "-t", "efivarfs", "efivarfs", efivars); err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("efivarfs mount failed (best-effort)", "err", err)
			}
		} else {
			mounted = append(mounted, efivars)
		}
	}

	cleanup := func() error {
		var firstErr error
		for i := len(mounted) - 1; i >= 0; i-- {
			if _, err := deps.Exec.Run(ctx, "umount", mounted[i]); err != nil {
				if deps.Logger != nil {
					deps.Logger.Warn("cleanup umount failed",
						"mount", mounted[i], "err", err)
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("install: umount %s: %w", mounted[i], err)
				}
			}
		}
		return firstErr
	}
	return espMount, cleanup, nil
}

// findESP enumerates partitions on parent and returns the first one
// whose blkid TYPE is vfat. We skip partDev itself (which we know is the
// root partition we just mounted) for speed.
// Returns ("", nil) when the disk has no ESP — callers treat this as a
// BIOS/MBR image and skip the UEFI bootloader path.
func findESP(ctx context.Context, deps Deps, parent, skip string) (string, error) {
	out, err := deps.Exec.Run(ctx, "lsblk", "-lnpo", "NAME,TYPE", parent)
	if err != nil {
		return "", fmt.Errorf("install: lsblk %s: %w", parent, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "part" {
			continue
		}
		name := fields[0]
		if name == skip {
			continue
		}
		typeOut, err := deps.Exec.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", name)
		if err != nil {
			// Unrecognised filesystem; skip.
			continue
		}
		if strings.TrimSpace(string(typeOut)) == "vfat" {
			return name, nil
		}
	}
	return "", nil
}

// parentDiskOf strips a trailing partition suffix from partDev:
//
//   /dev/sda3       -> /dev/sda
//   /dev/nvme0n1p3  -> /dev/nvme0n1
//   /dev/mmcblk0p1  -> /dev/mmcblk0
//
// If partDev has no numeric suffix it's returned unchanged.
func parentDiskOf(partDev string) string {
	end := len(partDev)
	// Trim trailing digits.
	for end > 0 && partDev[end-1] >= '0' && partDev[end-1] <= '9' {
		end--
	}
	if end == len(partDev) {
		return partDev
	}
	// Trim optional 'p' separator (nvme/mmcblk).
	if end > 0 && partDev[end-1] == 'p' {
		end--
	}
	return partDev[:end]
}

// mountFromFstab parses <mntRoot>/etc/fstab and mounts every entry whose
// mountpoint is neither "/" (already mounted) nor "swap"/"none" (not a real
// fs). Mountpoints are sorted by depth so parents are mounted before
// children (e.g. /boot before /boot/efi). Returns the list of successfully
// mounted targets (in mount order, for LIFO cleanup) and the absolute path
// of the ESP if /boot/efi was declared.
//
// The image's root is read but never written; if /etc/fstab is missing or
// unreadable we treat that as "image has no extra partitions" — current
// callers cover the no-fstab case via findESP. We don't fail Mount() on
// fstab parse errors: a malformed line is logged and skipped, because the
// goal is grub.cfg correctness, not strict validation. A failure to *mount*
// a declared partition does fail the install — silently dropping a
// declared /boot would just reproduce the bug we're fixing.
func mountFromFstab(ctx context.Context, deps Deps, mntRoot string) ([]string, string, error) {
	fstabPath := filepath.Join(mntRoot, "etc", "fstab")
	data, err := deps.FS.ReadFile(fstabPath)
	if err != nil {
		// Image without fstab (rare — mostly synthetic test images).
		// Caller will fall back to findESP for ESP discovery.
		return nil, "", nil
	}

	type entry struct {
		source string
		target string
	}
	var entries []entry
	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			if deps.Logger != nil {
				deps.Logger.Warn("fstab: skipping malformed line",
					"line", lineNo+1, "content", line)
			}
			continue
		}
		source, target, fstype := fields[0], fields[1], fields[2]
		if target == "/" || target == "swap" || target == "none" {
			continue
		}
		// Skip pseudo-fs entries that may be in the image's fstab.
		switch fstype {
		case "swap", "tmpfs", "proc", "sysfs", "devpts", "devtmpfs":
			continue
		}
		entries = append(entries, entry{source: source, target: target})
	}

	// Mount parents before children so /boot is in place before /boot/efi.
	sort.SliceStable(entries, func(i, j int) bool {
		return strings.Count(entries[i].target, "/") < strings.Count(entries[j].target, "/")
	})

	var mounted []string
	var espMount string
	for _, e := range entries {
		dev, err := resolveFstabSource(ctx, deps, e.source)
		if err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("fstab: source resolution failed; skipping",
					"source", e.source, "target", e.target, "err", err)
			}
			continue
		}
		if dev == "" {
			continue
		}
		target := filepath.Join(mntRoot, e.target)
		if err := deps.FS.MkdirAll(target, 0o755); err != nil {
			return mounted, "", fmt.Errorf("install: mkdir %s: %w", target, err)
		}
		if _, err := deps.Exec.Run(ctx, "mount", dev, target); err != nil {
			return mounted, "", fmt.Errorf("install: mount fstab %s (%s) -> %s: %w",
				e.source, dev, target, err)
		}
		mounted = append(mounted, target)
		if e.target == "/boot/efi" {
			espMount = target
		}
	}
	return mounted, espMount, nil
}

// resolveFstabSource turns an fstab source field into a concrete /dev path.
// LABEL= and UUID= are looked up via blkid; explicit /dev/... paths are
// returned as-is. Anything else (PARTUUID=, PARTLABEL=, network mounts) is
// resolved by blkid -t when applicable, otherwise returned empty so the
// caller skips the entry.
func resolveFstabSource(ctx context.Context, deps Deps, source string) (string, error) {
	if strings.HasPrefix(source, "/dev/") {
		return source, nil
	}
	// blkid -L / -U handle LABEL / UUID directly and print just the device.
	switch {
	case strings.HasPrefix(source, "LABEL="):
		out, err := deps.Exec.Run(ctx, "blkid", "-L", strings.TrimPrefix(source, "LABEL="))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	case strings.HasPrefix(source, "UUID="):
		out, err := deps.Exec.Run(ctx, "blkid", "-U", strings.TrimPrefix(source, "UUID="))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}
	// PARTUUID, PARTLABEL, network sources etc.: skip rather than guess.
	return "", nil
}
