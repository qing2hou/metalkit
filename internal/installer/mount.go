// mount.go assembles the chroot environment for the post-write steps
// (seed ISO copy, grub-install, update-grub). It mounts the just-grown
// root partition, then the ESP at /boot/efi, then bind-mounts the host's
// /proc /sys /dev /dev/pts /run inside, and finally efivars (best-effort).
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

	// Locate the ESP. We look for the first partition on the parent disk
	// whose blkid TYPE is vfat. We need the parent disk to enumerate
	// siblings; derive it from partDev by stripping trailing digits and
	// the optional 'p' separator.
	parent := parentDiskOf(partDev)
	espDev, err := findESP(ctx, deps, parent, partDev)
	if err != nil {
		rollback()
		return "", noop, err
	}
	espMount := filepath.Join(mntRoot, "boot", "efi")
	if err := deps.FS.MkdirAll(espMount, 0o755); err != nil {
		rollback()
		return "", noop, fmt.Errorf("install: mkdir %s: %w", espMount, err)
	}
	if _, err := deps.Exec.Run(ctx, "mount", espDev, espMount); err != nil {
		rollback()
		return "", noop, fmt.Errorf("install: mount ESP %s -> %s: %w", espDev, espMount, err)
	}
	mounted = append(mounted, espMount)

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

	// efivars is best-effort: failure here only means efibootmgr inside
	// the chroot won't be able to write NVRAM, but grub-install still
	// produces a working ESP that the BIOS can boot from the default
	// fallback BOOTX64.EFI path.
	efivars := filepath.Join(mntRoot, "sys", "firmware", "efi", "efivars")
	if _, err := deps.Exec.Run(ctx, "mount", "-t", "efivarfs", "efivarfs", efivars); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("efivarfs mount failed (best-effort)", "err", err)
		}
	} else {
		mounted = append(mounted, efivars)
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
	return "", fmt.Errorf("install: no FAT32 ESP partition found on %s", parent)
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
