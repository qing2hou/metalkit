// grub.go installs the UEFI bootloader into the freshly written root and
// refreshes /boot/grub/grub.cfg.
//
// Notes on cloud-init wiring: we previously dropped a
// /etc/default/grub.d/99-metalkit.cfg overlay that set
// `ds=nocloud-net;s=file:///boot/efi/` on the kernel cmdline so cloud-init's
// NoCloud datasource would find the seed ISO on the ESP. That whole hop is
// gone now — seed.go writes loose user-data / meta-data into
// /var/lib/cloud/seed/nocloud-net/ in the rootfs, which cloud-init
// auto-discovers without any cmdline assistance.
package installer

import (
	"context"
	"fmt"
)

// InstallGRUB chroots into mntRoot and:
//
//   1. Runs grub-install --target=x86_64-efi --efi-directory=/boot/efi.
//   2. Runs update-grub to regenerate /boot/grub/grub.cfg.
//   3. Best-effort efibootmgr -v for diagnostic logging.
//
// devPath is the parent disk; grub-install --recheck infers it from the
// efi-directory in practice but we pass it through for log context.
func InstallGRUB(ctx context.Context, deps Deps, mntRoot, devPath string) error {
	if mntRoot == "" {
		return fmt.Errorf("install: InstallGRUB: mntRoot is empty")
	}

	if _, err := deps.Exec.Run(ctx,
		"chroot", mntRoot,
		"grub-install",
		"--target=x86_64-efi",
		"--efi-directory=/boot/efi",
		"--bootloader-id=metalkit",
		"--recheck",
	); err != nil {
		return fmt.Errorf("install: grub-install (dev %s): %w", devPath, err)
	}

	if _, err := deps.Exec.Run(ctx, "chroot", mntRoot, "update-grub"); err != nil {
		return fmt.Errorf("install: update-grub: %w", err)
	}

	// efibootmgr is informational. We only log; on a system without
	// efivarfs (e.g. CI runner) it'll fail and that's fine.
	if out, err := deps.Exec.Run(ctx, "chroot", mntRoot, "efibootmgr", "-v"); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("efibootmgr -v failed (informational)", "err", err)
		}
	} else if deps.Logger != nil {
		deps.Logger.Info("efibootmgr -v", "output", string(out))
	}
	return nil
}
