// grub.go installs the bootloader into the freshly written root and
// refreshes /boot/grub/grub.cfg.
//
// Two strategies, picked by probing the target rootfs:
//
//  1. RHEL-family target with a chroot-able grub2-install present
//     (CentOS 7, Rocky 9, AlmaLinux, RHEL): we chroot into the target
//     and run THEIR grub2-install + grub2-mkconfig. This is the only
//     way to get a working EFI binary whose embedded prefix and module
//     versions match the on-disk /boot/grub2/* the target ships. Host
//     grub-install (Debian 12, grub 2.06+) writes a core image that
//     looks for /boot/grub/<arch>/normal.mod which Rocky doesn't have;
//     the result is a GRUB rescue prompt.
//
//  2. Anything else (Debian / Ubuntu cloud images, or a rootfs without
//     grub2-install): host grub-install + chroot update-grub fallbacks.
//     This is what we've always done and what works for the Ubuntu
//     22.04 cloud image.
//
// We bind-mount /proc /sys /dev /dev/pts /run before InstallGRUB runs
// (see mount.go), so chroot grub2-install can talk to efivars and resolve
// device paths.
//
// cloud-init wiring is unrelated and lives in seed.go.
package installer

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// InstallGRUB installs the bootloader into mntRoot. espMount is the
// mountpoint of the ESP inside mntRoot (e.g. "/boot/efi"); empty means
// BIOS. devPath is the parent disk device (e.g. /dev/sda).
//
// The implementation branches on whether the target rootfs ships its
// own grub2-install binary: yes → chroot install (RHEL family); no →
// host grub-install (Debian/Ubuntu cloud images).
func InstallGRUB(ctx context.Context, deps Deps, mntRoot, devPath, espMount string) error {
	if mntRoot == "" {
		return fmt.Errorf("install: InstallGRUB: mntRoot is empty")
	}

	if shouldChrootGrub2(deps, mntRoot) {
		return installGRUBChrootRHEL(ctx, deps, mntRoot, devPath, espMount)
	}
	return installGRUBHostDebian(ctx, deps, mntRoot, devPath, espMount)
}

// shouldChrootGrub2 reports whether to take the chroot grub2-install
// branch. Two conditions must hold:
//
//  1. The target rootfs ships /usr/sbin/grub2-install (or sbin/...).
//     RHEL-family images do; Debian/Ubuntu cloud images don't.
//  2. The target is a "modern" RHEL family (Rocky / AlmaLinux / RHEL 8+ /
//     Fedora). CentOS 7 also has grub2-install but is intentionally kept
//     on the host-grub path because that's what's been known-working in
//     production — we don't want to swap a tested path for an untested
//     one just because a binary is present.
//
// The combined gate keeps Ubuntu happy (no chroot binary), keeps CentOS 7
// happy (explicit ID/version filter), and fixes Rocky 9 (which lands here).
func shouldChrootGrub2(deps Deps, mntRoot string) bool {
	if !hasChrootGrub2Install(deps, mntRoot) {
		return false
	}
	data, err := deps.FS.ReadFile(filepath.Join(mntRoot, "etc", "os-release"))
	if err != nil {
		return false
	}
	c := string(data)
	// Explicit allowlist: modern RHEL-family that we know needs the
	// chroot path. Add new IDs here as we validate them on real hardware.
	for _, marker := range []string{
		`ID="rocky"`,
		`ID="almalinux"`,
		`ID="fedora"`,
	} {
		if strings.Contains(c, marker) {
			return true
		}
	}
	// RHEL itself: only 8+. RHEL 7 mirrors CentOS 7's quirks; keep it on
	// the host path until proven otherwise.
	if strings.Contains(c, `ID="rhel"`) && !strings.Contains(c, `VERSION_ID="7`) {
		return true
	}
	return false
}

// installGRUBChrootRHEL installs/registers the bootloader for a modern
// RHEL-family target (Rocky / AlmaLinux / RHEL 8+ / Fedora).
//
// UEFI path: we do NOT run grub2-install. RHEL's grub2-install refuses to
// run on EFI platforms ("This utility should not be used for EFI platforms
// because it does not support UEFI Secure Boot"). Cloud images already
// ship a complete EFI tree on the ESP — shim, grubx64.efi, BOOT/BOOTX64.EFI
// fallback, and a tiny grub.cfg under /EFI/<id>/ that chainloads
// /boot/grub2/grub.cfg via configfile. All we have to do is:
//
//  1. Regenerate /boot/grub2/grub.cfg from inside the chroot so the menu
//     entries reference the just-installed kernel(s) and the right
//     root=UUID.
//  2. Create an NVRAM boot entry pointing at /EFI/<id>/shimx64.efi so the
//     firmware actually loads it. Without this, the firmware falls back to
//     /EFI/BOOT/BOOTX64.EFI if it exists, but the SD/SATA boot order is
//     undefined — we want a deterministic "BootCurrent" entry.
//
// BIOS path: grub2-install is fine (no Secure Boot involvement). Chroot
// in so the modules embedded in the core image match the on-disk
// /boot/grub2/i386-pc.
func installGRUBChrootRHEL(ctx context.Context, deps Deps, mntRoot, devPath, espMount string) error {
	bootID := detectRHELBootloaderID(deps, mntRoot)

	if espMount != "" {
		// UEFI: use the image's pre-baked, signed EFI tree. No grub2-install.
		if err := registerEFIBootEntryRHEL(ctx, deps, mntRoot, devPath, bootID); err != nil {
			return err
		}
	} else {
		// BIOS: chroot grub2-install is safe and required (the host's grub
		// modules don't match).
		args := []string{
			mntRoot, "grub2-install",
			"--target=i386-pc",
			"--boot-directory=/boot",
			devPath,
		}
		if _, err := deps.Exec.Run(ctx, "chroot", args...); err != nil {
			return fmt.Errorf("install: chroot grub2-install (bios, dev %s): %w", devPath, err)
		}
	}

	// Regenerate /boot/grub2/grub.cfg from inside the target so menu
	// entries reference the target's own kernel paths and UUIDs.
	if _, err := deps.Exec.Run(ctx, "chroot", mntRoot, "grub2-mkconfig", "-o", "/boot/grub2/grub.cfg"); err != nil {
		// Don't hard-fail: the image's pre-baked grub.cfg may still be
		// usable; the EFI registration above is the load-bearing part.
		if deps.Logger != nil {
			deps.Logger.Warn("grub2-mkconfig (chroot) failed; relying on image's pre-baked grub.cfg", "err", err)
		}
	}

	// Bridge /boot/grub/grub.cfg → ../grub2/grub.cfg so any Debian-style
	// tooling on the live image (or recovery) finds the config.
	bridgeRHELGrubCfg(deps, mntRoot)

	if espMount != "" {
		if out, err := deps.Exec.Run(ctx, "efibootmgr", "-v"); err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("efibootmgr -v failed (informational)", "err", err)
			}
		} else if deps.Logger != nil {
			deps.Logger.Info("efibootmgr -v", "output", string(out))
		}
	}
	return nil
}

// registerEFIBootEntryRHEL creates a deterministic NVRAM entry pointing
// at the target's shim binary on the ESP, e.g.
//
//	efibootmgr -c -d /dev/sda -p 1 -L "Rocky Linux" -l '\EFI\rocky\shimx64.efi'
//
// Why shim and not grubx64.efi: RHEL family ships a Microsoft-signed shim
// that loads the (vendor-signed) grubx64.efi. Skipping shim means Secure
// Boot fails. The shim filename varies: shimx64.efi (newer), shim.efi
// (older Alma). We probe the ESP for what's actually there.
//
// Idempotency: if an entry with the same label and loader already exists
// (BootOrder + Boot####), we don't add a duplicate.
func registerEFIBootEntryRHEL(ctx context.Context, deps Deps, mntRoot, devPath, bootID string) error {
	espDir := filepath.Join(mntRoot, "boot", "efi", "EFI", bootID)
	if !deps.FS.Exists(espDir) {
		return fmt.Errorf("install: EFI dir %s missing on ESP — image did not ship a bootloader for id %q",
			espDir, bootID)
	}

	// Pick the loader file. Prefer shim (Secure Boot path), fall back to
	// grub directly if the image is shim-less (rare).
	var loaderFile string
	for _, cand := range []string{"shimx64.efi", "shim.efi", "grubx64.efi"} {
		if deps.FS.Exists(filepath.Join(espDir, cand)) {
			loaderFile = cand
			break
		}
	}
	if loaderFile == "" {
		return fmt.Errorf("install: no shim/grub binary found under %s", espDir)
	}
	loaderPath := `\EFI\` + bootID + `\` + loaderFile

	// efibootmgr wants the disk (e.g. /dev/sda) and the partition number
	// of the ESP. devPath is the parent disk; the ESP is whichever vfat
	// partition got mounted at /boot/efi. We resolve the partition number
	// from /proc/mounts so we don't have to re-run findESP.
	partNum, err := findESPPartNum(ctx, deps, mntRoot)
	if err != nil {
		return fmt.Errorf("install: locate ESP partition number: %w", err)
	}

	label := efiLabelFor(bootID)

	// Skip if an entry with the same label + loader already points at
	// this disk. RHEL cloud images sometimes ship pre-baked entries
	// referencing the source build host's NVRAM; those are stale, so
	// we still want OUR entry. Match on label+loader so we don't churn.
	if existing, err := deps.Exec.Run(ctx, "efibootmgr", "-v"); err == nil {
		if efiEntryMatches(string(existing), label, loaderFile) {
			if deps.Logger != nil {
				deps.Logger.Info("efibootmgr: matching entry already present, skipping create",
					"label", label, "loader", loaderPath)
			}
			return nil
		}
	}

	if _, err := deps.Exec.Run(ctx,
		"efibootmgr",
		"--create",
		"--disk", devPath,
		"--part", partNum,
		"--label", label,
		"--loader", loaderPath,
	); err != nil {
		return fmt.Errorf("install: efibootmgr --create (label %q loader %s): %w", label, loaderPath, err)
	}
	return nil
}

// efiLabelFor returns a human-readable NVRAM boot label for a given
// distro id. Falls back to titlecasing the id.
func efiLabelFor(bootID string) string {
	switch bootID {
	case "rocky":
		return "Rocky Linux"
	case "almalinux":
		return "AlmaLinux"
	case "redhat":
		return "Red Hat Enterprise Linux"
	case "centos":
		return "CentOS Linux"
	case "fedora":
		return "Fedora"
	}
	if bootID == "" {
		return "Linux"
	}
	return strings.ToUpper(bootID[:1]) + bootID[1:]
}

// findESPPartNum reads /proc/mounts to find which partition is mounted
// at <mntRoot>/boot/efi and returns its numeric partition number as a
// string (e.g. "2" for /dev/sda2, "1" for /dev/nvme0n1p1).
func findESPPartNum(ctx context.Context, deps Deps, mntRoot string) (string, error) {
	espMount := filepath.Join(mntRoot, "boot", "efi")
	data, err := deps.FS.ReadFile("/proc/mounts")
	if err != nil {
		return "", fmt.Errorf("read /proc/mounts: %w", err)
	}
	var espDev string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == espMount {
			espDev = fields[0]
			break
		}
	}
	if espDev == "" {
		return "", fmt.Errorf("no /proc/mounts entry for %s", espMount)
	}
	// Strip trailing digits — that's the partition number. Account for
	// nvme/mmcblk's "p" separator.
	end := len(espDev)
	for end > 0 && espDev[end-1] >= '0' && espDev[end-1] <= '9' {
		end--
	}
	if end == len(espDev) {
		return "", fmt.Errorf("device %s has no trailing partition number", espDev)
	}
	return espDev[end:], nil
}

// efiEntryMatches reports whether efibootmgr -v output already contains
// a Boot#### entry whose label matches `label` and whose loader path
// contains `loaderFile` (case-insensitive). Used to avoid duplicate NVRAM
// entries across reinstalls.
//
// efibootmgr -v format: `BootXXXX* <label>\t<HD(...)>/File(...)` — label
// can contain spaces; the tab separates it from the device path. We
// require a tab (or "  " two-space separator some efibootmgr variants
// emit) so we don't false-positive on labels embedded in another field.
var efiBootLineRE = regexp.MustCompile(`(?i)^Boot[0-9A-F]{4}\*?\s+(.*?)(?:\t|  )`)

func efiEntryMatches(efibootmgrOut, label, loaderFile string) bool {
	wantLabel := strings.ToLower(strings.TrimSpace(label))
	wantLoader := strings.ToLower(loaderFile)
	for _, line := range strings.Split(efibootmgrOut, "\n") {
		m := efiBootLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		gotLabel := strings.ToLower(strings.TrimSpace(m[1]))
		if gotLabel != wantLabel {
			continue
		}
		if strings.Contains(strings.ToLower(line), wantLoader) {
			return true
		}
	}
	return false
}

// installGRUBHostDebian uses the HOST's grub-install (Debian live image)
// to lay down a bootloader, then falls back through chroot update-grub
// variants. This is the original path and works for Debian/Ubuntu cloud
// images whose /boot/grub layout matches the host's grub-install output.
func installGRUBHostDebian(ctx context.Context, deps Deps, mntRoot, devPath, espMount string) error {
	bootDir := filepath.Join(mntRoot, "boot")
	efiDir := filepath.Join(mntRoot, "boot", "efi")

	if espMount != "" {
		if _, err := deps.Exec.Run(ctx,
			"grub-install",
			"--target=x86_64-efi",
			"--efi-directory="+efiDir,
			"--boot-directory="+bootDir,
			"--bootloader-id=metalkit",
			"--recheck",
		); err != nil {
			return fmt.Errorf("install: grub-install (uefi, dev %s): %w", devPath, err)
		}
	} else {
		if _, err := deps.Exec.Run(ctx,
			"grub-install",
			"--target=i386-pc",
			"--boot-directory="+bootDir,
			devPath,
		); err != nil {
			return fmt.Errorf("install: grub-install (bios, dev %s): %w", devPath, err)
		}
	}

	// update-grub: multi-tier fallback. Debian/Ubuntu have update-grub;
	// RHEL-family images we hit through this branch (no chroot
	// grub2-install) would still want grub2-mkconfig.
	updated := false
	for _, cmd := range [][]string{
		{"chroot", mntRoot, "update-grub"},
		{"chroot", mntRoot, "grub2-mkconfig", "-o", "/boot/grub2/grub.cfg"},
		{"chroot", mntRoot, "grub-mkconfig", "-o", "/boot/grub/grub.cfg"},
	} {
		if _, err := deps.Exec.Run(ctx, cmd[0], cmd[1:]...); err == nil {
			updated = true
			break
		}
	}
	if !updated {
		if _, err := deps.Exec.Run(ctx, "grub-mkconfig", "-o", bootDir+"/grub/grub.cfg"); err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("update-grub: all methods failed, continuing without regenerating grub.cfg",
					"err", err)
			}
		}
	}

	bridgeRHELGrubCfg(deps, mntRoot)

	if espMount != "" {
		if out, err := deps.Exec.Run(ctx, "efibootmgr", "-v"); err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("efibootmgr -v failed (informational)", "err", err)
			}
		} else if deps.Logger != nil {
			deps.Logger.Info("efibootmgr -v", "output", string(out))
		}
	}
	return nil
}

// hasChrootGrub2Install reports whether the target rootfs ships its own
// /usr/sbin/grub2-install (or sbin/grub2-install). RHEL-family images do;
// Debian/Ubuntu don't. Used to decide which InstallGRUB branch to take.
func hasChrootGrub2Install(deps Deps, mntRoot string) bool {
	for _, p := range []string{
		filepath.Join(mntRoot, "usr", "sbin", "grub2-install"),
		filepath.Join(mntRoot, "sbin", "grub2-install"),
	} {
		if deps.FS.Exists(p) {
			return true
		}
	}
	return false
}

// detectRHELBootloaderID returns the distro's expected EFI dir name
// under /boot/efi/EFI. We read /etc/os-release inside mntRoot. This
// matters because the embedded prefix of the shipped grubx64.efi
// references this dir; using a different id (e.g. "metalkit") would
// install a sibling EFI dir whose grubx64.efi has its OWN prefix pointing
// at the missing dir. Default "centos" preserves backward compat for the
// existing CentOS 7 install path.
func detectRHELBootloaderID(deps Deps, mntRoot string) string {
	data, err := deps.FS.ReadFile(filepath.Join(mntRoot, "etc", "os-release"))
	if err != nil {
		return "centos"
	}
	c := string(data)
	switch {
	case strings.Contains(c, `ID="rocky"`):
		return "rocky"
	case strings.Contains(c, `ID="almalinux"`):
		return "almalinux"
	case strings.Contains(c, `ID="rhel"`):
		return "redhat"
	case strings.Contains(c, `ID="fedora"`):
		return "fedora"
	case strings.Contains(c, `ID="centos"`):
		return "centos"
	}
	return "centos"
}

// bridgeRHELGrubCfg creates the legacy /boot/grub/grub.cfg → ../grub2/grub.cfg
// symlink when the image uses the RHEL layout and the target Debian-side
// path is missing. Idempotent and best-effort: failure only logs.
func bridgeRHELGrubCfg(deps Deps, mntRoot string) {
	grub2Cfg := filepath.Join(mntRoot, "boot", "grub2", "grub.cfg")
	grubCfg := filepath.Join(mntRoot, "boot", "grub", "grub.cfg")
	if _, err := deps.FS.Stat(grub2Cfg); err != nil {
		return
	}
	if _, err := deps.FS.Stat(grubCfg); err == nil {
		return
	}
	if err := deps.FS.Symlink("../grub2/grub.cfg", grubCfg); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("update-grub: symlink grub.cfg→grub2/grub.cfg failed", "err", err)
		}
	} else if deps.Logger != nil {
		deps.Logger.Info("update-grub: linked /boot/grub/grub.cfg → /boot/grub2/grub.cfg (RHEL layout)")
	}
}
