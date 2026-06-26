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
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"metalkit/internal/components"
	"metalkit/internal/jobs"
)

// InstallGRUB installs the bootloader into mntRoot. espMount is the
// mountpoint of the ESP inside mntRoot (e.g. "/boot/efi"); empty means
// BIOS. devPath is the parent disk device (e.g. /dev/sda).
//
// The implementation dispatches based on the bootloader selection from
// the profile (or auto-detected from the target rootfs). The auto-detect
// logic preserves the original behavior: chroot grub2-install for modern
// RHEL, host grub-install for Debian/Ubuntu.
func InstallGRUB(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath, espMount string) error {
	if mntRoot == "" {
		return fmt.Errorf("install: InstallGRUB: mntRoot is empty")
	}

	bl := resolveBootloader(spec.Bootloader, deps, mntRoot)
	switch bl {
	case components.GRUBHostDebian:
		return installGRUBHostDebian(ctx, deps, spec, mntRoot, devPath, espMount)
	case components.GRUBChrootRHEL:
		return installGRUBChrootRHEL(ctx, deps, spec, mntRoot, devPath, espMount)
	case components.GRUBHostFallback:
		return installGRUBHostFallback(ctx, deps, spec, mntRoot, devPath, espMount)
	default:
		return fmt.Errorf("install: unknown bootloader %q", bl)
	}
}

// resolveBootloader determines the effective bootloader strategy. If the
// profile has an explicit selection (non-empty), it wins. Otherwise we fall
// back to OS-detection heuristics that replicate the pre-component-selection
// behavior.
func resolveBootloader(explicit string, deps Deps, mntRoot string) components.Bootloader {
	if explicit != "" {
		return components.Bootloader(explicit)
	}
	// Auto-detect: replicate the old shouldChrootGrub2 logic.
	if shouldChrootGrub2(deps, mntRoot) {
		return components.GRUBChrootRHEL
	}
	return components.GRUBHostDebian
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
	// Match case-insensitively: openEuler ships ID="openEuler"
	// (capital E), while Rocky/Alma use all-lowercase IDs.
	cLower := strings.ToLower(c)
	for _, marker := range []string{
		`id="rocky"`,
		`id="almalinux"`,
		`id="fedora"`,
		`id="openeuler"`,
		`id="opensuse-leap"`,
		`id="opensuse-tumbleweed"`,
		`id="opensuse"`,
	} {
		if strings.Contains(cLower, marker) {
			return true
		}
	}
	// RHEL itself: only 8+. RHEL 7 mirrors CentOS 7's quirks; keep it on
	// the host path until proven otherwise.
	if strings.Contains(cLower, `id="rhel"`) && !strings.Contains(cLower, `version_id="7`) {
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
func installGRUBChrootRHEL(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath, espMount string) error {
	bootID := detectRHELBootloaderID(deps, mntRoot)

	if espMount != "" {
		// chrootEfiDir is the ESP path inside the chroot. Normally /boot/efi,
		// but openEuler mounts ESP at /boot (fstab: /boot vfat), so the
		// chroot sees it at /boot. We derive it from the absolute espMount
		// by stripping the mntRoot prefix.
		chrootEfiDir := "/boot/efi"
		if rel, err := filepath.Rel(mntRoot, espMount); err == nil && rel != "" && rel != "." {
			chrootEfiDir = "/" + rel
		}

		// UEFI: try using the image's pre-baked, signed EFI tree first.
		// If the image doesn't ship EFI files (some openEuler images),
		// fall back to grub-install which creates them.
		if err := registerEFIBootEntryRHEL(ctx, deps, mntRoot, devPath, bootID); err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("EFI boot entry registration failed, falling back to grub2-install", "err", err)
			}
			// Fall back: chroot grub2-install for UEFI. This works on
			// openEuler and distros that ship grub2-install but may not
			// have a pre-baked EFI tree.
			args := []string{
				mntRoot, "grub2-install",
				"--target=x86_64-efi",
				"--efi-directory=" + chrootEfiDir,
				"--boot-directory=/boot",
				"--bootloader-id=" + bootID,
				"--recheck",
				"--no-nvram",
			}
			if _, err2 := deps.Exec.Run(ctx, "chroot", args...); err2 != nil {
				return fmt.Errorf("install: EFI boot entry registration failed (%w) and chroot grub2-install also failed: %v", err, err2)
			}
			if deps.Logger != nil {
				deps.Logger.Info("chroot grub2-install (uefi fallback) succeeded")
			}
		}

		// Normalize the ESP directory layout. Some cloud images (openEuler
		// 20.03) nest the EFI tree one level too deep: efi/EFI/<bootID>/
		// instead of EFI/<bootID>/. The NVRAM loader path \EFI\<bootID>\
		// grubx64.efi resolves against the ESP root, so the firmware cannot
		// reach a file buried under efi/EFI/<bootID>/ — FAT32 is
		// case-insensitive but the path *levels* must still match. Copy the
		// bootloader files up one level so the firmware finds them.
		normalizeESPLayout(ctx, deps, espMount, bootID)
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

	// Regenerate grub.cfg from inside the target so menu entries
	// reference the target's own kernel paths and UUIDs. Try multiple
	// mkconfig variants and output paths — different distros use
	// different names and paths:
	//   Rocky/AlmaLinux/RHEL: grub2-mkconfig → /boot/grub2/grub.cfg
	//   openEuler:            grub2-mkconfig → /boot/grub2/grub.cfg (same)
	//   openSUSE:             grub2-mkconfig → /boot/grub2/grub.cfg
	//   Some distros:         grub-mkconfig  → /boot/grub/grub.cfg
	//
	// Ensure the output directories exist first.
	for _, dir := range []string{
		filepath.Join(mntRoot, "boot", "grub2"),
		filepath.Join(mntRoot, "boot", "grub"),
	} {
		if !deps.FS.Exists(dir) {
			_ = deps.FS.MkdirAll(dir, 0o755)
		}
	}

	updated := false
	for _, cmd := range [][]string{
		{"chroot", mntRoot, "grub2-mkconfig", "-o", "/boot/grub2/grub.cfg"},
		{"chroot", mntRoot, "grub2-mkconfig", "-o", "/boot/grub/grub.cfg"},
		{"chroot", mntRoot, "grub-mkconfig", "-o", "/boot/grub/grub.cfg"},
		{"chroot", mntRoot, "grub-mkconfig", "-o", "/boot/grub2/grub.cfg"},
	} {
		if _, err := deps.Exec.Run(ctx, cmd[0], cmd[1:]...); err == nil {
			updated = true
			if deps.Logger != nil {
				deps.Logger.Info("grub-mkconfig succeeded", "cmd", strings.Join(cmd, " "))
			}
			break
		}
	}
	if !updated {
		// All mkconfig methods failed. Try from the host as a last resort.
		for _, cmd := range [][]string{
			{"grub-mkconfig", "-o", filepath.Join(mntRoot, "boot", "grub", "grub.cfg")},
		} {
			if _, err := deps.Exec.Run(ctx, cmd[0], cmd[1:]...); err == nil {
				updated = true
				if deps.Logger != nil {
					deps.Logger.Info("host grub-mkconfig succeeded", "cmd", strings.Join(cmd, " "))
				}
				break
			}
		}
	}
	if !updated {
		if deps.Logger != nil {
			deps.Logger.Warn("grub-mkconfig: all methods failed, relying on image's pre-baked grub.cfg")
		}
	}

	// Bridge /boot/grub/grub.cfg → ../grub2/grub.cfg so any Debian-style
	// tooling on the live image (or recovery) finds the config.
	bridgeRHELGrubCfg(deps, mntRoot)

	// Regenerate initramfs in the chroot so it includes drivers for the
	// target hardware (e.g. megaraid_sas, mpt3sas, hpsa for Dell/HP RAID
	// controllers). Cloud images ship initramfs built for virtualized
	// hosts only, which fails to find the rootfs disk on physical
	// servers — boot drops into dracut emergency shell with
	// "/dev/disk/by-uuid/... does not exist".
	//
	// CRITICAL: this rebuild is gated on ensureKernelModulesInstalled
	// having actually installed new packages. Distros whose cloud images
	// already ship megaraid_sas + vfat (openEuler 24.03, RHEL 9 GenericCloud)
	// have a working initramfs out of the box — rebuilding it with
	// --no-hostonly can drop host-specific config and produce an
	// initramfs that boots worse than the original. Only Rocky 10
	// GenericCloud is known to need the rebuild (it strips kernel-modules
	// to shrink image size).
	modulesInstalled := regenerateInitramfsRHEL(ctx, deps, spec, mntRoot, devPath)

	// SELinux: cloud images ship SELinux=enforcing. Files we write into
	// the rootfs (NM keyfiles, cloud-init cfg, sshd_config.d, BLS entries,
	// grubenv, etc.) end up with unlabeled contexts (?), and SELinux
	// denies the corresponding services (NetworkManager,
	// systemd-modules-load, sshd) permission to read them — boot drops
	// to emergency with "Permission denied" on files that plainly exist.
	// Running setfiles/restorecon per-file is fragile (every new file we
	// write needs its context fixed). Disabling SELinux wholesale is the
	// pragmatic fix; users who need SELinux can re-enable it after first
	// boot + touch /.autorelabel. See qa.md #7.
	disableSELinuxIfEnforcing(deps, mntRoot)

	// Pin GRUB's default boot kernel to one whose /lib/modules has the
	// RAID/HBA drivers we just regenerated initramfs for. If `dnf install
	// kernel-modules` pulled in a newer kernel (because version-pinned
	// install failed and we fell back to unversioned), the older kernel
	// is still the GRUB default and its initramfs can't find the root
	// disk. See qa.md #10.
	//
	// Only run when we actually installed kernel-modules — otherwise the
	// image's existing default kernel selection is already correct, and
	// running grubby on a single-kernel image risks touching grubenv /
	// BLS entries in ways the image didn't expect.
	if modulesInstalled {
		setDefaultBootKernelToDriverComplete(ctx, deps, mntRoot)
	}

	// GRUB cmdline: cloud images default to `console=ttyS0,115200n8`
	// which redirects all kernel logs to the serial port. On physical
	// servers with no serial console attached, the local screen only
	// shows Plymouth animation — debugging boot failures is impossible
	// without manually editing the GRUB entry. Replace with
	// `console=tty0 nomodeset` so logs appear on the local console and
	// graphics mode doesn't mask them. See qa.md #9.
	fixRHELGrubCmdline(ctx, deps, mntRoot)

	// kdump: cloud images ship kdump enabled by default. On physical
	// servers kdump arming often fails (no reserved crashkernel memory,
	// missing kdump initramfs) and systemd treats the failure as a
	// dependency failure → emergency mode → root locked (cloud image
	// default) → user can't even log in to debug. Mask kdump to skip it
	// entirely. See qa.md #5.
	maskKdumpService(ctx, deps, mntRoot)

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
	// Find the EFI directory for this bootloader ID on the ESP. The ESP
	// mount point varies: /boot/efi (most distros) or /boot (openEuler).
	espDir := ""
	for _, base := range []string{
		filepath.Join(mntRoot, "boot", "efi", "EFI", bootID),
		filepath.Join(mntRoot, "boot", "EFI", bootID),
	} {
		if deps.FS.Exists(base) {
			espDir = base
			break
		}
	}
	if espDir == "" {
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

	// Delete any existing boot entries with the same label. After writing a
	// new image the partition's GPT PARTUUID changes, so stale NVRAM entries
	// from a previous install point at a non-existent partition and the
	// firmware falls through to whatever boots next (often an old Ubuntu).
	// Removing before recreating ensures the new entry is always fresh.
	if existing, err := deps.Exec.Run(ctx, "efibootmgr", "-v"); err == nil {
		removeStaleEFIEntries(ctx, deps, string(existing), label)
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

// removeStaleEFIEntries deletes all NVRAM boot entries whose label matches
// the given label. Used to clean up stale entries from previous installs
// before creating a fresh one.
func removeStaleEFIEntries(ctx context.Context, deps Deps, efibootmgrOut, label string) {
	wantLabel := strings.ToLower(strings.TrimSpace(label))
	for _, line := range strings.Split(efibootmgrOut, "\n") {
		m := efiBootLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		gotLabel := strings.ToLower(strings.TrimSpace(m[1]))
		if gotLabel != wantLabel {
			continue
		}
		// Extract the Boot#### index from the line.
		raw := strings.TrimSpace(strings.SplitN(line, " ", 2)[0])
		idx := strings.TrimPrefix(raw, "Boot")
		idx = strings.TrimSuffix(idx, "*")
		if len(idx) != 4 {
			continue
		}
		if deps.Logger != nil {
			deps.Logger.Info("efibootmgr: removing stale entry", "index", idx, "label", label)
		}
		if _, err := deps.Exec.Run(ctx, "efibootmgr", "-b", idx, "-B"); err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("efibootmgr: failed to remove stale entry", "index", idx, "err", err)
			}
		}
	}
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
	case "openeuler", "openEuler":
		return "openEuler"
	case "kylin":
		return "Kylin Linux"
	case "opensuse":
		return "openSUSE"
	}
	if bootID == "" {
		return "Linux"
	}
	return strings.ToUpper(bootID[:1]) + bootID[1:]
}

// findESPPartNum reads /proc/mounts to find which partition is mounted
// at the ESP mount point and returns its numeric partition number as a
// string (e.g. "2" for /dev/sda2, "1" for /dev/nvme0n1p1).
// Checks both /boot/efi (standard) and /boot (openEuler) paths.
func findESPPartNum(ctx context.Context, deps Deps, mntRoot string) (string, error) {
	data, err := deps.FS.ReadFile("/proc/mounts")
	if err != nil {
		return "", fmt.Errorf("read /proc/mounts: %w", err)
	}
	var espDev string
	for _, espCandidate := range []string{
		filepath.Join(mntRoot, "boot", "efi"),
		filepath.Join(mntRoot, "boot"),
	} {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == espCandidate {
				espDev = fields[0]
				break
			}
		}
		if espDev != "" {
			break
		}
	}
	if espDev == "" {
		return "", fmt.Errorf("no /proc/mounts entry for ESP under %s", mntRoot)
	}
	// Strip trailing digits - that is the partition number. Account for
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
func installGRUBHostDebian(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath, espMount string) error {
	bootDir := filepath.Join(mntRoot, "boot")
	efiDir := espMount
	if efiDir == "" || !strings.HasPrefix(efiDir, mntRoot) {
		efiDir = filepath.Join(mntRoot, "boot", "efi")
	}

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

	// Rebuild initramfs if the image's stock initramfs is missing storage
	// drivers needed on physical servers (Dell PERC = megaraid_sas, HP =
	// hpsa, LSI = mpt3sas). Debian/Ubuntu cloud images ship initramfs
	// built for virtio-only VMs; on a Dell R630 the kernel can't find
	// the RAID root disk and boot stalls after "Run /init as init
	// process" — ahci scans empty SATA ports then hangs waiting for a
	// rootfs that megaraid_sas would have provided. update-initramfs -u
	// rebuilds from the target's /etc/initramfs-tools config + installed
	// kernel-modules, picking up megaraid_sas if the package is present.
	//
	// Skip the standard-kernel install on non-RAID disks: cloud kernels
	// have ahci/nvme/virtio built in, so a pure SATA/NVMe target boots
	// fine without megaraid_sas & friends. Running apt-get install
	// linux-image-amd64 on a network-less live system would fail and
	// leave the user with a "succeeded" job that produces an unbootable
	// disk — but on non-RAID targets the cloud kernel IS bootable, so we
	// skip the install entirely and let the cloud kernel stay.
	regenerateInitramfsDebian(ctx, deps, spec, mntRoot, devPath)

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

// regenerateInitramfsDebian checks whether the target's initramfs has the
// critical storage drivers (megaraid_sas for Dell PERC, hpsa for HP, etc.)
// and rebuilds it via update-initramfs if any are missing. Best-effort:
// failures are logged but don't abort the install.
//
// Debian cloud images (e.g. debian-13-genericcloud) ship a `-cloud-amd64`
// kernel that strips RAID/HBA drivers to shrink image size — on a Dell
// R630 with PERC H730, the kernel can't find the root disk and boot
// stalls after "Run /init as init process". The fix is to install the
// full `linux-image-amd64` meta-package, which pulls a standard kernel
// with all SCSI drivers, then rebuild the initramfs.
//
// This mirrors the RHEL path's ensureKernelModulesInstalled logic but
// uses apt + update-initramfs instead of dnf + dracut.
//
// devPath is the target disk (/dev/sda). When the target is NOT behind a
// RAID/HBA controller (pure SATA/NVMe/virtio), the cloud kernel's built-in
// ahci/nvme/virtio drivers are enough to boot — we skip the standard-kernel
// install entirely. This makes Debian 13/14 installs work on non-RAID
// servers even when the live system has no internet access (apt install
// would fail and leave the user with an unbootable disk).
func regenerateInitramfsDebian(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath string) {
	criticalDrivers := []string{"megaraid_sas", "mpt3sas", "hpsa", "aacraid", "smartpqi"}
	kvers := listKernelVersions(deps, mntRoot)
	if len(kvers) == 0 {
		if deps.Logger != nil {
			deps.Logger.Warn("regenerate-initramfs-debian: no kernels found under /lib/modules, skipping")
		}
		return
	}

	// Check whether each kernel's /lib/modules has at least one critical
	// driver. If every kernel has all of them, the stock initramfs is
	// already good — skip to avoid replacing a known-working initramfs.
	missing := map[string]bool{}
	for _, kver := range kvers {
		modDir := filepath.Join(mntRoot, "lib", "modules", kver)
		for _, drv := range criticalDrivers {
			out, _ := deps.Exec.Run(ctx, "find", modDir, "-name", drv+".ko*", "-print", "-quit")
			if strings.TrimSpace(string(out)) == "" {
				if deps.Logger != nil {
					deps.Logger.Warn("regenerate-initramfs-debian: driver MISSING from image",
						"driver", drv, "kernel", kver)
				}
				missing[drv] = true
			}
		}
	}
	if len(missing) == 0 {
		if deps.Logger != nil {
			deps.Logger.Info("regenerate-initramfs-debian: all critical drivers present, skipping rebuild")
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "info", "initramfs rebuild skipped — Debian image already has storage drivers")
		}
		return
	}

	// The cloud kernel is missing RAID drivers. Check whether the target
	// disk is actually behind a RAID/HBA controller. If not (pure SATA,
	// NVMe, or virtio), the cloud kernel's built-in ahci/nvme/virtio
	// drivers can boot the disk fine — the missing RAID drivers are
	// irrelevant. Skip the standard-kernel install so the install
	// succeeds without network.
	if !isRAIDControllerDisk(ctx, deps, devPath) {
		if deps.Logger != nil {
			deps.Logger.Info("regenerate-initramfs-debian: target disk not on RAID controller, cloud kernel's ahci/nvme sufficient — skipping standard-kernel install",
				"dev", devPath)
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "info",
				fmt.Sprintf("skipped standard-kernel install: %s is not behind a RAID controller (cloud kernel's ahci/nvme drivers suffice)", devPath))
		}
		return
	}

	missingList := make([]string, 0, len(missing))
	for d := range missing {
		missingList = append(missingList, d)
	}
	if deps.Reporter != nil {
		_ = deps.Reporter.Log(ctx, "info",
			fmt.Sprintf("installing full kernel for missing storage drivers: %s", strings.Join(missingList, ", ")))
	}

	// Write a working resolv.conf so apt can resolve mirrors. Same logic
	// as the RHEL path: the live system's /etc/resolv.conf may be a
	// symlink to systemd-resolved stub that doesn't work in chroot.
	dnsServers := spec.Profile.ChrootDNS
	if len(dnsServers) == 0 {
		dnsServers = []string{"223.5.5.5", "114.114.114.114"}
	}
	resolvTarget := filepath.Join(mntRoot, "etc", "resolv.conf")
	_ = deps.FS.MkdirAll(filepath.Dir(resolvTarget), 0o755)
	_ = deps.FS.Remove(resolvTarget)
	var sb strings.Builder
	for _, s := range dnsServers {
		sb.WriteString("nameserver ")
		sb.WriteString(s)
		sb.WriteString("\n")
	}
	if err := deps.FS.WriteFile(resolvTarget, []byte(sb.String()), 0o644); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("regenerate-initramfs-debian: failed to write resolv.conf", "err", err)
		}
	}

	// Point apt at TUNA (Tsinghua mirror) for fast downloads from China.
	// Debian cloud images default to deb.debian.org which is slow from
	// CN networks and can time out mid-install. We rewrite the mirror
	// files in-place; the sources.list uses mirror+file:/// so changing
	// the mirror file is sufficient. Best-effort: if the file layout
	// differs (older Debian releases), apt update just uses the default.
	mirrorDir := filepath.Join(mntRoot, "etc", "apt", "mirrors")
	_ = deps.FS.MkdirAll(mirrorDir, 0o755)
	tunaDebian := []byte("https://mirrors.tuna.tsinghua.edu.cn/debian\n")
	tunaSecurity := []byte("https://mirrors.tuna.tsinghua.edu.cn/debian-security\n")
	_ = deps.FS.WriteFile(filepath.Join(mirrorDir, "debian.list"), tunaDebian, 0o644)
	_ = deps.FS.WriteFile(filepath.Join(mirrorDir, "debian-security.list"), tunaSecurity, 0o644)
	if deps.Logger != nil {
		deps.Logger.Info("regenerate-initramfs-debian: switched apt mirror to TUNA")
	}

	// Install the full kernel meta-package. apt will pull a standard
	// linux-image-amd64 (with all SCSI drivers) alongside the existing
	// cloud kernel. We don't remove the cloud kernel — GRUB picks the
	// highest version by default, and the new standard kernel will
	// outrank the cloud one. setDefaultBootKernelToDriverComplete is
	// called below to make sure we boot the driver-complete kernel.
	installArgs := []string{
		mntRoot, "apt-get", "update",
	}
	if out, err := deps.Exec.Run(ctx, "chroot", installArgs...); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("regenerate-initramfs-debian: apt-get update failed",
				"err", err, "out", string(out))
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "warn",
				fmt.Sprintf("apt-get update failed: %v — initramfs may stay incomplete", err))
		}
		// Continue anyway — local apt cache may still have the package.
	}

	installArgs = []string{
		mntRoot, "apt-get", "install", "-y",
		"linux-image-amd64",
	}
	if out, err := deps.Exec.Run(ctx, "chroot", installArgs...); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("regenerate-initramfs-debian: apt-get install linux-image-amd64 failed",
				"err", err, "out", string(out))
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "warn",
				fmt.Sprintf("linux-image-amd64 install failed: %v (output: %s) — initramfs may lack storage drivers", err, string(out)))
		}
		return
	}
	if deps.Reporter != nil {
		_ = deps.Reporter.Log(ctx, "info", "linux-image-amd64 installed (full SCSI driver set)")
	}

	// Re-enumerate kernels after install — the new standard kernel is
	// now in /lib/modules alongside the cloud one.
	newKvers := listKernelVersions(deps, mntRoot)

	// Rebuild initramfs for every kernel. update-initramfs -u -k all
	// handles all installed kernels in one shot.
	rebuildArgs := []string{mntRoot, "update-initramfs", "-u", "-k", "all"}
	if out, err := deps.Exec.Run(ctx, "chroot", rebuildArgs...); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("regenerate-initramfs-debian: update-initramfs failed",
				"err", err, "out", string(out))
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "warn",
				fmt.Sprintf("initramfs rebuild failed: %v (output: %s)", err, string(out)))
		}
	} else {
		if deps.Logger != nil {
			deps.Logger.Info("regenerate-initramfs-debian: update-initramfs succeeded",
				"kernels", strings.Join(newKvers, ","))
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "info",
				fmt.Sprintf("initramfs rebuilt for kernels: %s", strings.Join(newKvers, ", ")))
		}
	}

	// Regenerate grub.cfg so the new standard kernel's menuentry is
	// present. The grub.cfg generated earlier in installGRUBHostDebian
	// predates the apt install and only knows about the cloud kernel —
	// without this, GRUB_DEFAULT=0 boots the cloud kernel and the boot
	// stalls after "Run /init as init process".
	//
	// Before pinning, purge the cloud kernel so GRUB has only one kernel
	// to choose from. This is the most reliable fix: we tried
	// GRUB_DEFAULT=saved + saved_entry=<menuentry-id> but GRUB on Debian
	// 13/14 still booted the cloud kernel (root cause never fully
	// isolated — likely a combination of ESP embedded config search.fs_uuid
	// failing on disk-order changes and saved_entry semantics). Purging
	// the cloud kernel removes the ambiguity entirely: grub.cfg ends up
	// with exactly one menuentry (the standard amd64 kernel), and any
	// GRUB default (0, saved, title) picks it.
	purgeCloudKernelDebian(ctx, deps, mntRoot)

	pinDebianDefaultKernel(ctx, deps, mntRoot)
}

// purgeCloudKernelDebian removes the cloud kernel package(s) whose
// isRAIDControllerDisk reports whether devPath (/dev/sda) sits behind a
// hardware RAID or HBA controller that needs a dedicated kernel driver
// (megaraid_sas, mpt3sas, hpsa, aacraid, smartpqi). Returns false for
// plain SATA/AHCI disks, NVMe, USB, and virtio — those use kernel drivers
// (ahci, nvme, usb-storage, virtio_blk) that ship with every cloud kernel.
//
// Detection walks the sysfs tree from /sys/block/<dev>/device up to find
// the driver module name. On a Dell PERC H730, the path resolves to
// /sys/bus/pci/drivers/megaraid_sas/, so we see "megaraid_sas" as the
// driver. On plain AHCI, it resolves to /sys/bus/pci/drivers/ahci/.
//
// We check the driver name against the critical RAID driver list. If any
// match, the disk is behind a RAID controller and needs that driver in
// the initramfs to boot.
//
// Fallback when sysfs is unreadable (live image without sysfs, unusual
// device layouts): conservatively return true. Better to attempt the
// standard-kernel install and fail than to skip it and produce an
// unbootable disk on a real RAID controller.
func isRAIDControllerDisk(ctx context.Context, deps Deps, devPath string) bool {
	if devPath == "" {
		return true // conservative
	}
	base := filepath.Base(devPath)
	// /sys/block/sda/device/driver -> symlink to the driver module dir.
	// readlink -f to resolve. Some disks (NVMe) have device at a different
	// path; we try a few candidates.
	candidates := []string{
		"/sys/block/" + base + "/device/driver",
		"/sys/block/" + base + "/device/../driver",
	}
	raidDrivers := map[string]bool{
		"megaraid_sas": true,
		"mpt3sas":      true,
		"hpsa":         true,
		"aacraid":      true,
		"smartpqi":     true,
		"cciss":        true, // older HP Smart Array
		"arcmsr":       true, // Areca
	}
	for _, link := range candidates {
		out, err := deps.Exec.Run(ctx, "readlink", "-f", link)
		if err != nil {
			continue
		}
		resolved := strings.TrimSpace(string(out))
		if resolved == "" {
			continue
		}
		driverName := filepath.Base(resolved)
		if raidDrivers[driverName] {
			if deps.Logger != nil {
				deps.Logger.Info("raid-detect: disk is on RAID controller",
					"dev", devPath, "driver", driverName)
			}
			return true
		}
		// Also check the driver symlink's path components for the driver
		// name (covers /sys/bus/pci/drivers/megaraid_sas/ form).
		lower := strings.ToLower(resolved)
		for drv := range raidDrivers {
			if strings.Contains(lower, drv) {
				if deps.Logger != nil {
					deps.Logger.Info("raid-detect: disk is on RAID controller (path match)",
						"dev", devPath, "driver", drv, "path", resolved)
				}
				return true
			}
		}
		if deps.Logger != nil {
			deps.Logger.Info("raid-detect: disk NOT on RAID controller",
				"dev", devPath, "driver", driverName)
		}
		return false
	}
	// Could not determine driver — conservative: assume RAID.
	if deps.Logger != nil {
		deps.Logger.Warn("raid-detect: could not read driver from sysfs, assuming RAID (conservative)",
			"dev", devPath)
	}
	return true
}

// purgeCloudKernelDebian removes the cloud kernel package(s) whose
// /lib/modules/<kver> lacks the critical RAID drivers, leaving only the
// standard kernel installed by linux-image-amd64. This eliminates the
// GRUB default-kernel ambiguity that caused Debian 13/14 to boot the
// driver-stripped cloud kernel.
//
// We identify cloud kernels by the same scoring logic as
// setDefaultBootKernelToDriverComplete: a kernel is "cloud" (driver-stripped)
// if its /lib/modules is missing any of the critical RAID/HBA drivers.
// Kernels with all drivers present (the standard linux-image-amd64 kernel)
// are kept.
//
// Best-effort: apt purge failures are logged but don't abort. If purge
// fails, pinDebianDefaultKernel's GRUB_DEFAULT=saved fallback still runs.
func purgeCloudKernelDebian(ctx context.Context, deps Deps, mntRoot string) {
	criticalDrivers := []string{"megaraid_sas", "mpt3sas", "hpsa", "aacraid", "smartpqi"}
	kvers := listKernelVersions(deps, mntRoot)
	if len(kvers) <= 1 {
		// Only one kernel installed — nothing to purge (either the image
		// already had the standard kernel, or linux-image-amd64 failed to
		// install and we'd be purging the only bootable kernel).
		return
	}

	// Score each kernel. Cloud kernels (driver-stripped) score low;
	// standard kernels score high. Purge the low scorers.
	type kcand struct {
		kver  string
		score int
	}
	var cands []kcand
	for _, kver := range kvers {
		score := 0
		modDir := filepath.Join(mntRoot, "lib", "modules", kver)
		for _, drv := range criticalDrivers {
			out, _ := deps.Exec.Run(ctx, "find", modDir, "-name", drv+".ko*", "-print", "-quit")
			if strings.TrimSpace(string(out)) != "" {
				score++
			}
		}
		cands = append(cands, kcand{kver, score})
		if deps.Logger != nil {
			deps.Logger.Info("purge-cloud-kernel: candidate scored",
				"kernel", kver, "score", score, "of", len(criticalDrivers))
		}
	}

	// Find at least one kernel with a perfect score (all drivers). We
	// only purge cloud kernels if a driver-complete kernel exists to
	// replace them — otherwise we'd leave the system unbootable.
	hasComplete := false
	for _, c := range cands {
		if c.score == len(criticalDrivers) {
			hasComplete = true
			break
		}
	}
	if !hasComplete {
		if deps.Logger != nil {
			deps.Logger.Warn("purge-cloud-kernel: no driver-complete kernel found, keeping cloud kernel")
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "warn",
				"no driver-complete kernel available; cloud kernel kept (boot may stall on RAID controllers)")
		}
		return
	}

	// Purge each driver-stripped kernel. Two-step:
	//  1. apt-get purge the linux-image-<kver> / linux-modules-<kver> /
	//     linux-modules-extra-<kver> packages (cleans dpkg metadata).
	//  2. Manually rm /boot/{vmlinuz,initrd.img,System.map,config}-<kver>
	//     and /lib/modules/<kver>/ (removes files dpkg doesn't own).
	//
	// Step 2 is CRITICAL: Debian cloud images bake the cloud kernel files
	// directly into the image, NOT via dpkg. apt-get purge returns "Unable
	// to locate package" (no dpkg entry), but the files remain in /boot.
	// update-grub scans /boot and still finds vmlinuz-<cloud-kver>, emits
	// a menuentry for it, and GRUB picks it first (cloud kernel sorts
	// lexically higher: "cloud-amd64" > "amd64"). The result: boot stalls
	// or kernel panics because the cloud kernel lacks megaraid_sas.
	//
	// apt-get purge is still worth running for cleanliness on images where
	// the cloud kernel IS dpkg-managed (Debian 13), but the manual rm is
	// what guarantees the files are gone regardless of image build method.
	var purged []string
	for _, c := range cands {
		if c.score == len(criticalDrivers) {
			continue // keep the good one
		}
		// Step 1: apt purge (best-effort, tolerate missing packages).
		for _, pkg := range []string{
			"linux-image-" + c.kver,
			"linux-modules-" + c.kver,
			"linux-modules-extra-" + c.kver,
		} {
			purgeArgs := []string{mntRoot, "apt-get", "purge", "-y", pkg}
			out, err := deps.Exec.Run(ctx, "chroot", purgeArgs...)
			if err != nil {
				combined := string(out) + " " + err.Error()
				if strings.Contains(combined, "Unable to locate package") ||
					strings.Contains(combined, "is not installed") ||
					strings.Contains(combined, "Package is not installed") {
					continue
				}
				if deps.Logger != nil {
					deps.Logger.Warn("purge-cloud-kernel: apt purge failed",
						"pkg", pkg, "err", err, "out", string(out))
				}
				continue
			}
		}

		// Step 2: manually remove kernel files dpkg doesn't own. Cloud
		// images bake these in directly; apt purge can't touch them.
		removedFiles := false
		for _, rel := range []string{
			"boot/vmlinuz-" + c.kver,
			"boot/initrd.img-" + c.kver,
			"boot/System.map-" + c.kver,
			"boot/config-" + c.kver,
		} {
			full := filepath.Join(mntRoot, rel)
			if deps.FS.Exists(full) {
				if err := deps.FS.Remove(full); err != nil {
					if deps.Logger != nil {
						deps.Logger.Warn("purge-cloud-kernel: failed to remove file",
							"path", rel, "err", err)
					}
				} else {
					removedFiles = true
				}
			}
		}
		modDir := filepath.Join(mntRoot, "lib", "modules", c.kver)
		if deps.FS.Exists(modDir) {
			if _, err := deps.Exec.Run(ctx, "rm", "-rf", modDir); err != nil {
				if deps.Logger != nil {
					deps.Logger.Warn("purge-cloud-kernel: failed to remove modules dir",
						"kver", c.kver, "err", err)
				}
			} else {
				removedFiles = true
			}
		}
		if removedFiles {
			purged = append(purged, c.kver)
		} else if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "warn",
				fmt.Sprintf("failed to purge cloud kernel %s (no files removed)", c.kver))
		}
	}
	if len(purged) > 0 {
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "info",
				fmt.Sprintf("purged driver-stripped cloud kernels: %s", strings.Join(purged, ", ")))
		}
		// Regenerate grub.cfg now that the cloud kernel files are gone —
		// grub.cfg will have exactly one menuentry (the standard kernel).
		if out, err := deps.Exec.Run(ctx, "chroot", mntRoot, "update-grub"); err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("purge-cloud-kernel: update-grub after purge failed",
					"err", err, "out", string(out))
			}
		}
	}
}

// pinDebianDefaultKernel picks the standard kernel whose /lib/modules has
// the RAID drivers (megaraid_sas, etc.), then makes GRUB boot it by:
//   1. running `chroot update-grub` so the new kernel's menuentry appears
//   2. setting GRUB_DEFAULT=saved in /etc/default/grub (Debian cloud images
//      default to 0, which would pick the cloud kernel that sorts higher
//      lexically: "cloud-amd64" > "amd64")
//   3. re-running update-grub so grub.cfg emits the saved_entry lookup
//   4. writing saved_entry=<menuentry-title> to grubenv
//
// On Debian, saved_entry is the menuentry TITLE (e.g.
// "Debian GNU/Linux, with Linux 6.12.90+deb13.1-amd64"), NOT the kernel
// version — Debian's grub.cfg uses menuentry ids matching titles, unlike
// RHEL's BLS snippets which key on version. We grep grub.cfg to extract
// the title for the kernel whose `linux` line points at vmlinuz-<kver>.
func pinDebianDefaultKernel(ctx context.Context, deps Deps, mntRoot string) {
	criticalDrivers := []string{"megaraid_sas", "mpt3sas", "hpsa", "aacraid", "smartpqi", "vfat"}
	kvers := listKernelVersions(deps, mntRoot)
	bestKver := ""
	bestScore := -1
	for _, kver := range kvers {
		score := 0
		modDir := filepath.Join(mntRoot, "lib", "modules", kver)
		for _, drv := range criticalDrivers {
			out, _ := deps.Exec.Run(ctx, "find", modDir, "-name", drv+".ko*", "-print", "-quit")
			if strings.TrimSpace(string(out)) != "" {
				score++
			}
		}
		if score > bestScore || (score == bestScore && kver > bestKver) {
			bestKver = kver
			bestScore = score
		}
		if deps.Logger != nil {
			deps.Logger.Info("pin-debian-kernel: candidate scored",
				"kernel", kver, "score", score, "of", len(criticalDrivers))
		}
	}
	if bestKver == "" {
		if deps.Logger != nil {
			deps.Logger.Warn("pin-debian-kernel: no kernel candidates found, skipping")
		}
		return
	}
	if deps.Reporter != nil {
		_ = deps.Reporter.Log(ctx, "info",
			fmt.Sprintf("pin-debian-kernel: best kernel %s (driver score %d/%d)",
				bestKver, bestScore, len(criticalDrivers)))
	}

	// Step 1: regenerate grub.cfg with the new kernel's menuentry.
	if out, err := deps.Exec.Run(ctx, "chroot", mntRoot, "update-grub"); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("pin-debian-kernel: update-grub failed",
				"err", err, "out", string(out))
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "warn",
				fmt.Sprintf("update-grub failed: %v — grub.cfg may be missing the new kernel menuentry", err))
		}
		// Continue anyway — grub.cfg may have been regenerated by the
		// kernel package's postinst trigger; we just don't know.
	} else {
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "info", "grub.cfg regenerated with new kernel menuentry")
		}
	}

	// Step 2: ensure /etc/default/grub uses GRUB_DEFAULT=saved so the
	// saved_entry we write below is honored at boot. Debian cloud images
	// ship GRUB_DEFAULT=0 which boots whichever menuentry sorts first —
	// typically the cloud kernel.
	grubDefaultPath := filepath.Join(mntRoot, "etc", "default", "grub")
	if data, rerr := deps.FS.ReadFile(grubDefaultPath); rerr == nil {
		lines := strings.Split(string(data), "\n")
		modified := false
		found := false
		for i, ln := range lines {
			if strings.HasPrefix(ln, "GRUB_DEFAULT=") {
				found = true
				if !strings.HasPrefix(ln, "GRUB_DEFAULT=saved") {
					lines[i] = "GRUB_DEFAULT=saved"
					modified = true
				}
				break
			}
		}
		if !found {
			lines = append([]string{"GRUB_DEFAULT=saved"}, lines...)
			modified = true
		}
		if modified {
			if werr := deps.FS.WriteFile(grubDefaultPath,
				[]byte(strings.Join(lines, "\n")), 0o644); werr != nil {
				if deps.Logger != nil {
					deps.Logger.Warn("pin-debian-kernel: failed to set GRUB_DEFAULT=saved", "err", werr)
				}
			} else if _, err := deps.Exec.Run(ctx, "chroot", mntRoot, "update-grub"); err != nil {
				if deps.Logger != nil {
					deps.Logger.Warn("pin-debian-kernel: update-grub after GRUB_DEFAULT=saved failed",
						"err", err)
				}
			} else if deps.Reporter != nil {
				_ = deps.Reporter.Log(ctx, "info",
					"GRUB_DEFAULT=saved written, grub.cfg regenerated")
			}
		}
	}

	// Step 3: find the menuentry ID for the best kernel in grub.cfg.
	// Debian's grub.cfg has entries like:
	//   menuentry 'Debian GNU/Linux, with Linux 6.12.90+deb13.1-amd64' --class ... $menuentry_id_option 'gnulinux-6.12.90+deb13.1-amd64-advanced-<uuid>' {
	//       load_video
	//       ...
	//       linux /boot/vmlinuz-6.12.90+deb13.1-amd64 root=UUID=...
	//       initrd /boot/initrd.img-6.12.90+deb13.1-amd64
	//   }
	// We grep for the menuentry whose linux line points at vmlinuz-<bestKver>
	// and extract its ID (the quoted string after $menuentry_id_option).
	//
	// CRITICAL: GRUB's saved_entry matches the menuentry ID, NOT the title.
	// grub-set-default does NOT translate title→ID — it writes whatever
	// string you pass directly into grubenv. So passing the title leaves
	// saved_entry=<title>, GRUB can't match it to any entry's ID, and it
	// falls back to default=0 (the cloud kernel, which sorts first). This
	// was the root cause of Debian 13 booting the cloud kernel despite the
	// agent logging "GRUB saved_entry set to <title>".
	entryID := findMenuentryIDForKernel(deps, mntRoot, bestKver)
	if entryID == "" {
		if deps.Logger != nil {
			deps.Logger.Warn("pin-debian-kernel: could not find menuentry ID for kernel",
				"kver", bestKver)
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "warn",
				fmt.Sprintf("could not pin default kernel to %s — no menuentry ID found in grub.cfg", bestKver))
		}
		return
	}

	// Step 4: set saved_entry via grub-set-default (writes grubenv with
	// correct quoting). Fallback to writing grubenv directly if
	// grub-set-default isn't available or fails.
	setArgs := []string{mntRoot, "grub-set-default", entryID}
	if out, err := deps.Exec.Run(ctx, "chroot", setArgs...); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("pin-debian-kernel: grub-set-default failed, writing grubenv directly",
				"err", err, "out", string(out))
		}
		grubenvPath := filepath.Join(mntRoot, "boot", "grub", "grubenv")
		envData, eerr := deps.FS.ReadFile(grubenvPath)
		if eerr != nil {
			// Try grub2 layout.
			grubenvPath = filepath.Join(mntRoot, "boot", "grub2", "grubenv")
			envData, eerr = deps.FS.ReadFile(grubenvPath)
		}
		if eerr != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("pin-debian-kernel: grubenv not found, cannot set saved_entry", "err", eerr)
			}
			return
		}
		envLines := strings.Split(string(envData), "\n")
		wrote := false
		for i, ln := range envLines {
			if strings.HasPrefix(ln, "saved_entry=") {
				envLines[i] = "saved_entry=" + entryID
				wrote = true
				break
			}
		}
		if !wrote {
			for i, ln := range envLines {
				if !strings.HasPrefix(ln, "#") && strings.TrimSpace(ln) != "" {
					envLines = append(envLines[:i],
						append([]string{"saved_entry=" + entryID}, envLines[i:]...)...)
					wrote = true
					break
				}
			}
		}
		if !wrote {
			envLines = append(envLines, "saved_entry="+entryID)
		}
		if werr := deps.FS.WriteFile(grubenvPath,
			[]byte(strings.Join(envLines, "\n")), 0o644); werr != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("pin-debian-kernel: failed to write grubenv", "err", werr)
			}
			return
		}
	}
	if deps.Reporter != nil {
		_ = deps.Reporter.Log(ctx, "info",
			fmt.Sprintf("GRUB saved_entry set to %q (kernel %s, driver score %d/%d)",
				entryID, bestKver, bestScore, len(criticalDrivers)))
	}
}

// findMenuentryIDForKernel reads grub.cfg from the installed rootfs and
// returns the menuentry ID (the quoted string after $menuentry_id_option)
// of the menuentry whose `linux` line points at vmlinuz-<kver>. Returns ""
// if not found. Handles both /boot/grub/grub.cfg (Debian) and
// /boot/grub2/grub.cfg (RHEL family) layouts.
//
// Example grub.cfg line we parse:
//   menuentry 'Debian GNU/Linux, with Linux 6.12.90+deb13.1-amd64' --class debian ... $menuentry_id_option 'gnulinux-6.12.90+deb13.1-amd64-advanced-98f79480-...' {
//
// We extract the LAST single-quoted string on the menuentry line (the ID),
// not the first (the title). This is what GRUB matches saved_entry against.
func findMenuentryIDForKernel(deps Deps, mntRoot, kver string) string {
	for _, relPath := range []string{"boot/grub/grub.cfg", "boot/grub2/grub.cfg"} {
		data, err := deps.FS.ReadFile(filepath.Join(mntRoot, relPath))
		if err != nil {
			continue
		}
		// Walk menuentry blocks. A menuentry block opens with
		//   menuentry 'Title' ... $menuentry_id_option 'ID' {
		// and closes with `}` on its own line. Inside, the linux line is
		//   linux /boot/vmlinuz-<kver> ...
		// We collect the most recently seen menuentry ID and check
		// whether the linux line inside matches vmlinuz-<kver>.
		var currentID string
		inEntry := false
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if !inEntry {
				if i := strings.Index(trimmed, "menuentry "); i >= 0 {
					rest := trimmed[i+len("menuentry "):]
					currentID = extractMenuentryID(rest)
					inEntry = true
					continue
				}
				continue
			}
			// Inside the menuentry block.
			if trimmed == "}" {
				inEntry = false
				currentID = ""
				continue
			}
			if strings.HasPrefix(trimmed, "linux ") || strings.HasPrefix(trimmed, "linux\t") {
				if strings.Contains(trimmed, "vmlinuz-"+kver+" ") ||
					strings.HasSuffix(trimmed, "vmlinuz-"+kver) {
					return currentID
				}
			}
		}
	}
	return ""
}

// extractMenuentryID parses a menuentry line (everything after "menuentry ")
// and returns the menuentry ID — the quoted string immediately following
// $menuentry_id_option. Returns "" if no ID is found.
//
// The line looks like:
//   'Title' --class debian ... $menuentry_id_option 'gnulinux-...-advanced-<uuid>' {
//
// We find $menuentry_id_option and grab the next single-quoted string. On
// older GRUB without the option, we fall back to "" (GRUB will use the
// title as the ID, which is what grub-set-default also does in that case).
func extractMenuentryID(line string) string {
	idx := strings.Index(line, "$menuentry_id_option")
	if idx < 0 {
		return ""
	}
	rest := line[idx+len("$menuentry_id_option"):]
	rest = strings.TrimLeft(rest, " \t")
	if len(rest) == 0 {
		return ""
	}
	// ID is in single quotes (Debian) or double quotes (some configs).
	q := rest[0:1]
	if q != "'" && q != "\"" {
		return ""
	}
	end := strings.Index(rest[1:], q)
	if end < 0 {
		return ""
	}
	return rest[1 : 1+end]
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
	cLower := strings.ToLower(c)
	switch {
	case strings.Contains(cLower, `id="rocky"`):
		return "rocky"
	case strings.Contains(cLower, `id="almalinux"`):
		return "almalinux"
	case strings.Contains(cLower, `id="rhel"`):
		return "redhat"
	case strings.Contains(cLower, `id="fedora"`):
		return "fedora"
	case strings.Contains(cLower, `id="centos"`):
		return "centos"
	case strings.Contains(cLower, `id="openeuler"`):
		// openEuler's EFI directory uses capital E: /EFI/openEuler/
		// The actual os-release has ID="openEuler" (capital E).
		// We must return the EXACT case that appears on the ESP.
		return "openEuler"
	case strings.Contains(cLower, `id="kylin"`):
		return "kylin"
	case strings.Contains(cLower, `id="opensuse-leap"`):
		return "opensuse"
	case strings.Contains(cLower, `id="opensuse-tumbleweed"`):
		return "opensuse"
	case strings.Contains(cLower, `id="opensuse"`):
		return "opensuse"
	}
	return "centos"
}

// installGRUBHostFallback is the generic fallback bootloader path. It uses
// the host's grub-install and tries multiple update-grub variants. This is
// identical to installGRUBHostDebian but exists as a separate function so
// the component-selection dispatch can target it explicitly when the operator
// wants the safe fallback regardless of OS detection.
func installGRUBHostFallback(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath, espMount string) error {
	return installGRUBHostDebian(ctx, deps, spec, mntRoot, devPath, espMount)
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

// regenerateInitramfsRHEL rebuilds initramfs in the chroot with --no-hostonly
// so it includes drivers for any hardware the target may boot on. Cloud
// images ship initramfs targeted at virtual machines (virtio only), which
// leaves physical servers with RAID controllers (Dell PERC: megaraid_sas;
// HP Smart Array: hpsa; LSI: mpt3sas) unable to find the rootfs disk and
// drops to dracut emergency shell.
//
// Two-pass strategy:
//   Pass 1: Detect missing critical drivers (megaraid_sas, vfat, etc.) under
//           /lib/modules/<KVER>/. If missing, chroot `dnf install -y
//           kernel-modules kernel-modules-extra` to pull the full driver set.
//           (Rocky/RHEL 10 GenericCloud omits this package by default.)
//   Pass 2: For each kernel, `dracut --no-hostonly --force --force-drivers
//           "megaraid_sas mpt3sas hpsa aacraid smartpqi nvme vfat fat"`
//           --force-drivers (vs --add-drivers) makes dracut write
//           /etc/cmdline.d/20-force_drivers.conf in the initramfs so the
//           drivers are insmod'd during early boot — not just available on
//           the filesystem. This matters for vfat: the kernel needs vfat
//           loaded BEFORE systemd tries to mount /boot/efi, and
//           systemd-modules-load in the rootfs is too late (plus SELinux
//           may deny access to /etc/modules-load.d files, see qa.md #7 #8).
//
// Best-effort: a failure here is logged but does not abort the install,
// since some targets (e.g. fully baked images) may not need it.
// regenerateInitramfsRHEL rebuilds initramfs in the chroot with --no-hostonly
// so it includes drivers for any hardware the target may boot on. Returns
// true if the rebuild actually happened (i.e. kernel-modules was installed
// AND dracut ran for at least one kernel); false when the rebuild was
// skipped because the image already had the required drivers, or when no
// kernels were found. Callers use the return value to gate
// setDefaultBootKernelToDriverComplete.
func regenerateInitramfsRHEL(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath string) bool {
	installed := ensureKernelModulesInstalled(ctx, deps, spec, mntRoot, devPath)
	if !installed {
		if deps.Logger != nil {
			deps.Logger.Info("regenerate-initramfs: kernel-modules already present, keeping image's original initramfs")
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "info", "initramfs rebuild skipped — image already has required drivers")
		}
		return false
	}

	kvers := listKernelVersions(deps, mntRoot)
	if len(kvers) == 0 {
		if deps.Logger != nil {
			deps.Logger.Warn("regenerate-initramfs: no kernels found under /lib/modules, skipping")
		}
		return false
	}
	for _, kver := range kvers {
		// Sanity check: log which storage drivers actually exist on
		// disk for this kernel. dracut --no-hostonly can only include
		// drivers that are present in /lib/modules/<kver>/. If the
		// cloud image stripped them out we need to know.
		if deps.Logger != nil {
			modDir := filepath.Join(mntRoot, "lib", "modules", kver)
			for _, drv := range []string{"megaraid_sas", "mpt3sas", "hpsa", "aacraid", "smartpqi", "vfat"} {
				out, _ := deps.Exec.Run(ctx, "find", modDir, "-name", drv+".ko*", "-print", "-quit")
				if strings.TrimSpace(string(out)) != "" {
					deps.Logger.Info("regenerate-initramfs: driver present", "driver", drv, "path", strings.TrimSpace(string(out)))
				} else {
					deps.Logger.Warn("regenerate-initramfs: driver MISSING from image", "driver", drv, "kernel", kver)
				}
			}
		}
		// dracut --no-hostonly: include all available drivers.
		// --force-drivers: write to /etc/cmdline.d/20-force_drivers.conf
		//                 in initramfs so drivers are insmod'd at early
		//                 boot. Critical for vfat (boot-efi mount) and
		//                 megaraid_sas (rootfs disk discovery).
		// --add-drivers: also include in initramfs module tree (idempotent
		//                with --force-drivers but explicit).
		// -f: overwrite existing initramfs.
		args := []string{
			mntRoot, "dracut", "--no-hostonly", "--force",
			"--force-drivers", "megaraid_sas mpt3sas hpsa aacraid smartpqi nvme vfat fat",
			"--add-drivers", "megaraid_sas mpt3sas hpsa aacraid smartpqi nvme vfat fat",
			filepath.Join("/boot", "initramfs-"+kver+".img"),
			kver,
		}
		if out, err := deps.Exec.Run(ctx, "chroot", args...); err == nil {
			if deps.Logger != nil {
				deps.Logger.Info("regenerate-initramfs: dracut succeeded", "kernel", kver)
			}
			if deps.Reporter != nil {
				_ = deps.Reporter.Log(ctx, "info", "initramfs regenerated for kernel "+kver)
			}
			continue
		} else {
			if deps.Logger != nil {
				deps.Logger.Warn("regenerate-initramfs: dracut failed, trying mkinitrd", "kernel", kver, "err", err, "out", string(out))
			}
			if deps.Reporter != nil {
				_ = deps.Reporter.Log(ctx, "warn", fmt.Sprintf("dracut failed for kernel %s: %v (output: %s)", kver, err, string(out)))
			}
		}
		// mkinitrd fallback (older RHEL/CentOS, some openEuler).
		mkArgs := []string{
			mntRoot, "mkinitrd", "-f",
			filepath.Join("/boot", "initramfs-"+kver+".img"),
			kver,
		}
		if out, err := deps.Exec.Run(ctx, "chroot", mkArgs...); err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("regenerate-initramfs: mkinitrd also failed", "kernel", kver, "err", err)
			}
			if deps.Reporter != nil {
				_ = deps.Reporter.Log(ctx, "error", fmt.Sprintf("mkinitrd also failed for kernel %s: %v (output: %s)", kver, err, string(out)))
			}
		} else if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "info", "initramfs regenerated via mkinitrd for kernel "+kver)
		}
	}
	return true
}

// ensureKernelModulesInstalled detects whether megaraid_sas.ko (and vfat.ko)
// are present in the target's /lib/modules/. Rocky/RHEL 10 GenericCloud
// ships with only kernel-core + kernel-modules-core, omitting the full
// kernel-modules package that contains RAID/HBA drivers needed on physical
// servers. If missing, attempt `dnf install -y kernel-modules
// kernel-modules-extra` in the chroot.
//
// This is best-effort: requires network access from the chroot (we already
// copied /etc/resolv.conf into the target during Mount). Failure is logged
// but not fatal — the image may already be a MetalKit-prepared one (see
// IMAGE-PREP.md) where this step is unnecessary.
//
// Returns true if kernel-modules packages were actually installed (i.e. a
// dnf transaction succeeded). Returns false when no install was needed
// (drivers already present) or when the install failed. Callers use the
// return value to decide whether to rebuild the initramfs — a rebuild
// without a fresh kernel-modules install is pointless and risks replacing
// a working image-shipped initramfs with one that drops host-specific
// config.
func ensureKernelModulesInstalled(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath string) bool {
	missing := []string{}
	for _, drv := range []string{"megaraid_sas", "vfat"} {
		// Check all kernel version dirs under /lib/modules/.
		modulesDir := filepath.Join(mntRoot, "lib", "modules")
		entries, err := os.ReadDir(modulesDir)
		if err != nil {
			continue
		}
		found := false
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			out, _ := deps.Exec.Run(ctx, "find",
				filepath.Join(modulesDir, e.Name()),
				"-name", drv+".ko*", "-print", "-quit")
			if strings.TrimSpace(string(out)) != "" {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, drv)
		}
	}
	if len(missing) == 0 {
		if deps.Logger != nil {
			deps.Logger.Info("ensure-kernel-modules: all critical drivers present, skipping dnf install")
		}
		return false
	}
	// The image is missing RAID drivers. Check whether the target disk is
	// actually behind a RAID/HBA controller. If not (pure SATA/NVMe/
	// virtio), the cloud kernel's built-in ahci/nvme/virtio drivers can
	// boot the disk fine — skip dnf install so the install succeeds
	// without network. See regenerateInitramfsDebian for the same guard.
	if !isRAIDControllerDisk(ctx, deps, devPath) {
		if deps.Logger != nil {
			deps.Logger.Info("ensure-kernel-modules: target disk not on RAID controller, cloud kernel sufficient — skipping dnf install",
				"dev", devPath, "missing", missing)
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "info",
				fmt.Sprintf("skipped kernel-modules install: %s is not behind a RAID controller (missing drivers %v not needed)", devPath, missing))
		}
		return false
	}
	if deps.Logger != nil {
		deps.Logger.Warn("ensure-kernel-modules: critical drivers missing, installing kernel-modules package",
			"missing", missing)
	}
	if deps.Reporter != nil {
		_ = deps.Reporter.Log(ctx, "info",
			fmt.Sprintf("installing kernel-modules package (missing drivers: %s)", strings.Join(missing, ", ")))
	}
	// chroot dnf needs DNS to reach the distro mirror. The live system's
	// /etc/resolv.conf may be a symlink to systemd-resolved's stub
	// (127.0.0.53) which works in chroot only if /run is bind-mounted
	// AND systemd-resolved is running. To be robust against any live
	// environment, write a static resolv.conf.
	//
	// DNS servers come from the profile's chroot_dns field (set via the
	// web platform). Empty list → installer defaults (China-friendly
	// public DNS: 223.5.5.5 + 114.114.114.114).
	dnsServers := spec.Profile.ChrootDNS
	if len(dnsServers) == 0 {
		dnsServers = []string{"223.5.5.5", "114.114.114.114"}
	}
	resolvTarget := filepath.Join(mntRoot, "etc", "resolv.conf")
	_ = deps.FS.MkdirAll(filepath.Dir(resolvTarget), 0o755)
	// Remove any existing symlink first (Rocky cloud image ships
	// /etc/resolv.conf as a symlink to ../run/systemd/resolve/stub-resolv.conf).
	_ = deps.FS.Remove(resolvTarget)
	var sb strings.Builder
	for _, s := range dnsServers {
		sb.WriteString("nameserver ")
		sb.WriteString(s)
		sb.WriteString("\n")
	}
	if err := deps.FS.WriteFile(resolvTarget, []byte(sb.String()), 0o644); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("ensure-kernel-modules: failed to write resolv.conf", "err", err)
		}
	} else if deps.Logger != nil {
		deps.Logger.Info("ensure-kernel-modules: wrote resolv.conf", "dns", dnsServers)
	}
	// Version-pin the install to the kernel(s) already on disk. Without a
	// version suffix, `dnf install kernel-modules kernel-modules-extra`
	// resolves to the latest available kernel-modules package, which pulls
	// in a matching newer kernel + kernel-core. The old kernel then still
	// lacks megaraid_sas in /lib/modules/<old-kver>/, but GRUB keeps it as
	// the default boot entry (dnf does not bump default_kernel) → boot
	// drops into dracut emergency because the old initramfs can't find
	// the RAID root disk. See qa.md #10.
	existingKvers := listKernelVersions(deps, mntRoot)
	var installArgs []string
	if len(existingKvers) == 1 {
		kver := existingKvers[0]
		installArgs = []string{
			"dnf", "install", "-y",
			"kernel-modules-" + kver,
			"kernel-modules-extra-" + kver,
		}
		if deps.Logger != nil {
			deps.Logger.Info("ensure-kernel-modules: version-pinned install",
				"kernel", kver)
		}
	} else {
		// Multiple kernels (or none detected) — fall back to unversioned
		// install. setDefaultBootKernelToDriverComplete runs later to
		// pick a boot kernel whose /lib/modules has the RAID drivers.
		installArgs = []string{"dnf", "install", "-y", "kernel-modules", "kernel-modules-extra"}
		if deps.Logger != nil {
			deps.Logger.Info("ensure-kernel-modules: unversioned install (multi/no kernel pre-existing)")
		}
	}
	chrootArgs := append(append([]string{}, "chroot", mntRoot), installArgs...)
	out, err := deps.Exec.Run(ctx, chrootArgs[0], chrootArgs[1:]...)
	if err != nil {
		// Versioned install can fail if the exact kernel-modules-<kver>
		// package isn't in the configured repo (older point release
		// superseded). Retry with unversioned packages so we at least
		// pull modules for whatever kernel the repo offers; the
		// set-default-kernel step later ensures we boot a driver-complete
		// kernel.
		if len(existingKvers) == 1 {
			if deps.Logger != nil {
				deps.Logger.Warn("ensure-kernel-modules: versioned install failed, retrying unversioned",
					"err", err, "out", string(out))
			}
			installArgs = []string{"dnf", "install", "-y", "kernel-modules", "kernel-modules-extra"}
			chrootArgs = append(append([]string{}, "chroot", mntRoot), installArgs...)
			out, err = deps.Exec.Run(ctx, chrootArgs[0], chrootArgs[1:]...)
		}
	}
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("ensure-kernel-modules: dnf install failed (image may not have network or repos)",
				"err", err, "out", string(out))
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "warn",
				fmt.Sprintf("kernel-modules install failed: %v (output: %s) — initramfs may be incomplete", err, string(out)))
		}
		return false
	}
	if deps.Logger != nil {
		deps.Logger.Info("ensure-kernel-modules: dnf install succeeded")
	}
	if deps.Reporter != nil {
		_ = deps.Reporter.Log(ctx, "info", "kernel-modules + kernel-modules-extra installed")
	}
	return true
}

// setDefaultBootKernelToDriverComplete ensures GRUB's default boot kernel is
// one whose /lib/modules/<kver>/ actually contains megaraid_sas.ko (and the
// other RAID/HBA drivers we care about). This is the safety net for the case
// where `dnf install kernel-modules` pulled in a newer kernel whose modules
// are complete while the older (still-default) kernel's modules remain
// stripped — without this, GRUB boots the old kernel, its initramfs lacks
// megaraid_sas, and switch-root fails because the RAID root disk isn't
// discovered. See qa.md #10.
//
// Strategy: enumerate /lib/modules/<kver>/, score each by how many of the
// critical drivers are present as .ko files, pick the highest-scoring (ties
// broken by lexical = newer version), and call `grubby --set-default` on it.
// If grubby is unavailable, write saved_entry directly to grubenv.
func setDefaultBootKernelToDriverComplete(ctx context.Context, deps Deps, mntRoot string) {
	criticalDrivers := []string{"megaraid_sas", "mpt3sas", "hpsa", "aacraid", "smartpqi", "vfat"}
	kvers := listKernelVersions(deps, mntRoot)
	if len(kvers) == 0 {
		if deps.Logger != nil {
			deps.Logger.Warn("set-default-kernel: no kernels found under /lib/modules, skipping")
		}
		return
	}
	type candidate struct {
		kver  string
		score int
	}
	best := candidate{"", -1}
	for _, kver := range kvers {
		score := 0
		modDir := filepath.Join(mntRoot, "lib", "modules", kver)
		for _, drv := range criticalDrivers {
			out, _ := deps.Exec.Run(ctx, "find", modDir, "-name", drv+".ko*", "-print", "-quit")
			if strings.TrimSpace(string(out)) != "" {
				score++
			}
		}
		// Prefer higher score; on ties, lexicographically larger kver
		// (newer version) wins.
		if score > best.score || (score == best.score && kver > best.kver) {
			best = candidate{kver, score}
		}
		if deps.Logger != nil {
			deps.Logger.Info("set-default-kernel: candidate scored",
				"kernel", kver, "score", score, "of", len(criticalDrivers))
		}
	}
	if best.kver == "" {
		if deps.Logger != nil {
			deps.Logger.Warn("set-default-kernel: no candidate kernel had any critical driver, skipping")
		}
		return
	}
	if best.score < len(criticalDrivers) {
		if deps.Logger != nil {
			deps.Logger.Warn("set-default-kernel: best kernel still missing some drivers",
				"kernel", best.kver, "score", best.score, "of", len(criticalDrivers))
		}
	}
	vmlinuzPath := "/boot/vmlinuz-" + best.kver
	out, err := deps.Exec.Run(ctx, "chroot", mntRoot,
		"grubby", "--set-default="+vmlinuzPath)
	if err == nil {
		if deps.Logger != nil {
			deps.Logger.Info("set-default-kernel: grubby set default",
				"kernel", best.kver, "vmlinuz", vmlinuzPath, "score", best.score)
		}
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "info",
				fmt.Sprintf("default boot kernel set to %s (driver score %d/%d)",
					best.kver, best.score, len(criticalDrivers)))
		}
		return
	}
	if deps.Logger != nil {
		deps.Logger.Warn("set-default-kernel: grubby --set-default failed, falling back to grubenv",
			"err", err, "out", string(out))
	}
	// Fallback: write saved_entry into grubenv. grubby reads this on boot.
	// Format: saved_entry=<index-or-title>. We use the BLS title which is
	// the kernel version string for RHEL family.
	grubenvPath := filepath.Join(mntRoot, "boot", "grub2", "grubenv")
	data, rerr := deps.FS.ReadFile(grubenvPath)
	if rerr != nil {
		// Try /boot/grub/grubenv (Debian layout) as a last resort.
		grubenvPath = filepath.Join(mntRoot, "boot", "grub", "grubenv")
		data, rerr = deps.FS.ReadFile(grubenvPath)
	}
	if rerr != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("set-default-kernel: grubenv not found, cannot fallback",
				"err", rerr)
		}
		return
	}
	lines := strings.Split(string(data), "\n")
	wrote := false
	for i, ln := range lines {
		if strings.HasPrefix(ln, "saved_entry=") {
			lines[i] = "saved_entry=" + best.kver
			wrote = true
			break
		}
	}
	if !wrote {
		// Append after the header comment block.
		for i, ln := range lines {
			if !strings.HasPrefix(ln, "#") && strings.TrimSpace(ln) != "" {
				lines = append(lines[:i], append([]string{"saved_entry=" + best.kver}, lines[i:]...)...)
				wrote = true
				break
			}
		}
	}
	if !wrote {
		lines = append(lines, "saved_entry="+best.kver)
	}
	if werr := deps.FS.WriteFile(grubenvPath, []byte(strings.Join(lines, "\n")), 0o644); werr != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("set-default-kernel: failed to write grubenv", "err", werr)
		}
		return
	}
	if deps.Logger != nil {
		deps.Logger.Info("set-default-kernel: wrote saved_entry to grubenv (grubby fallback)",
			"kernel", best.kver, "path", grubenvPath)
	}
	if deps.Reporter != nil {
		_ = deps.Reporter.Log(ctx, "info",
			fmt.Sprintf("default boot kernel set via grubenv to %s (driver score %d/%d)",
				best.kver, best.score, len(criticalDrivers)))
	}
}

// listKernelVersions reads <mntRoot>/lib/modules/* and returns the set of
// kernel version directories present. Order is unspecified.
func listKernelVersions(deps Deps, mntRoot string) []string {
	modulesDir := filepath.Join(mntRoot, "lib", "modules")
	entries, err := os.ReadDir(modulesDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// fixRHELGrubCmdline rewrites /etc/default/grub's GRUB_CMDLINE_LINUX_DEFAULT
// to replace `console=ttyS0,115200n8` with `console=tty0 nomodeset`, then
// propagates the change to every BLS entry under /boot/loader/entries/ via
// `grubby --update-kernel ALL`. Without this, cloud images default to
// serial console output and the local screen shows only Plymouth — boot
// failures are invisible without manual GRUB editing. See qa.md #9.
//
// If /etc/default/grub doesn't exist (non-RHEL target), this is a no-op.
// If grubby is missing or fails, we fall back to sed-replacing the options
// line directly in each BLS entry file.
func fixRHELGrubCmdline(ctx context.Context, deps Deps, mntRoot string) {
	defaultGrub := filepath.Join(mntRoot, "etc", "default", "grub")
	data, err := deps.FS.ReadFile(defaultGrub)
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.Info("fix-grub-cmdline: no /etc/default/grub in target, skipping")
		}
		return
	}
	lines := strings.Split(string(data), "\n")
	changed := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "GRUB_CMDLINE_LINUX_DEFAULT=") {
			continue
		}
		val := strings.TrimPrefix(t, "GRUB_CMDLINE_LINUX_DEFAULT=")
		val = strings.Trim(val, `"'`)
		// Replace serial console with local console + nomodeset.
		val = strings.ReplaceAll(val, "console=ttyS0,115200n8", "console=tty0 nomodeset")
		// Also handle the case where only console=ttyS0 appears without
		// the baud spec, and ensure nomodeset is present.
		val = strings.ReplaceAll(val, "console=ttyS0", "console=tty0")
		if !strings.Contains(val, "nomodeset") {
			val = "nomodeset " + val
		}
		lines[i] = `GRUB_CMDLINE_LINUX_DEFAULT="` + val + `"`
		changed = true
	}
	if !changed {
		if deps.Logger != nil {
			deps.Logger.Info("fix-grub-cmdline: no GRUB_CMDLINE_LINUX_DEFAULT line, no change")
		}
		return
	}
	if err := deps.FS.WriteFile(defaultGrub, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("fix-grub-cmdline: failed to write /etc/default/grub", "err", err)
		}
		return
	}
	if deps.Logger != nil {
		deps.Logger.Info("fix-grub-cmdline: rewrote GRUB_CMDLINE_LINUX_DEFAULT to use console=tty0 nomodeset")
	}
	// Propagate to BLS entries via grubby. grubby reads
	// /etc/default/grub and rewrites the `options` line in every
	// /boot/loader/entries/*.conf to match. If grubby is unavailable or
	// fails (some minimal images lack it), fall back to sed-replacing
	// the options line in each BLS entry file directly.
	if out, err := deps.Exec.Run(ctx, "chroot", mntRoot,
		"grubby", "--update-kernel=ALL", "--args=console=tty0 nomodeset",
		"--remove-args=console=ttyS0,115200n8"); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("fix-grub-cmdline: grubby failed, falling back to direct BLS edit",
				"err", err, "out", string(out))
		}
		fixBLSEntriesCmdline(deps, mntRoot)
	} else if deps.Reporter != nil {
		_ = deps.Reporter.Log(ctx, "info", "GRUB cmdline updated: console=tty0 nomodeset")
	}
}

// fixBLSEntriesCmdline is the fallback for when grubby is unavailable:
// sed-replace the options line in each /boot/loader/entries/*.conf file.
func fixBLSEntriesCmdline(deps Deps, mntRoot string) {
	entriesDir := filepath.Join(mntRoot, "boot", "loader", "entries")
	if !deps.FS.Exists(entriesDir) {
		if deps.Logger != nil {
			deps.Logger.Warn("fix-grub-cmdline: no /boot/loader/entries dir, cannot fallback")
		}
		return
	}
	// List BLS entries via find (FS interface has no ReadDir). In a real
	// install this runs on the live system so `find` is always available.
	out, err := deps.Exec.Run(context.Background(), "find", entriesDir,
		"-maxdepth", "1", "-name", "*.conf", "-type", "f")
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("fix-grub-cmdline: find BLS entries failed", "err", err)
		}
		return
	}
	count := 0
	for _, p := range strings.Fields(string(out)) {
		data, err := deps.FS.ReadFile(p)
		if err != nil {
			continue
		}
		content := string(data)
		content = strings.ReplaceAll(content, "console=ttyS0,115200n8", "console=tty0 nomodeset")
		content = strings.ReplaceAll(content, "console=ttyS0", "console=tty0")
		// Ensure nomodeset is in any options line.
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(line, "options ") && !strings.Contains(line, "nomodeset") {
				content = strings.Replace(content, line, "nomodeset "+line, 1)
			}
		}
		_ = deps.FS.WriteFile(p, []byte(content), 0o644)
		count++
	}
	if deps.Logger != nil {
		deps.Logger.Info("fix-grub-cmdline: BLS entries rewritten directly (grubby fallback)",
			"count", count)
	}
}

// disableSELinuxIfEnforcing reads /etc/selinux/config in the target rootfs
// and flips SELINUX=enforcing → SELINUX=disabled. Idempotent: no-op if
// already disabled or permissive. See the comment on
// installGRUBChrootRHEL for why this is necessary.
func disableSELinuxIfEnforcing(deps Deps, mntRoot string) {
	configPath := filepath.Join(mntRoot, "etc", "selinux", "config")
	data, err := deps.FS.ReadFile(configPath)
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.Info("disable-selinux: no /etc/selinux/config in target, skipping")
		}
		return
	}
	lines := strings.Split(string(data), "\n")
	changed := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "SELINUX=") {
			val := strings.TrimSpace(strings.TrimPrefix(t, "SELINUX="))
			if val == "enforcing" || val == "permissive" {
				lines[i] = "SELINUX=disabled"
				changed = true
			}
		}
	}
	if !changed {
		if deps.Logger != nil {
			deps.Logger.Info("disable-selinux: already disabled, no change")
		}
		return
	}
	if err := deps.FS.WriteFile(configPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("disable-selinux: failed to write /etc/selinux/config", "err", err)
		}
		return
	}
	if deps.Logger != nil {
		deps.Logger.Info("disable-selinux: set SELINUX=disabled")
	}
}

// maskKdumpService creates /etc/systemd/system/kdump.service → /dev/null
// symlink in the target rootfs to fully disable kdump. systemctl mask in
// chroot would normally do this, but chroot doesn't run systemd so we
// write the symlink directly. Idempotent: no-op if already masked.
func maskKdumpService(ctx context.Context, deps Deps, mntRoot string) {
	maskPath := filepath.Join(mntRoot, "etc", "systemd", "system", "kdump.service")
	systemdDir := filepath.Dir(maskPath)
	if err := deps.FS.MkdirAll(systemdDir, 0o755); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("mask-kdump: mkdir systemd dir failed", "err", err)
		}
		return
	}
	// If already masked (symlink to /dev/null), no-op.
	if info, err := deps.FS.Stat(maskPath); err == nil {
		_ = info
		if deps.Logger != nil {
			deps.Logger.Info("mask-kdump: already masked, no change")
		}
		return
	}
	// Remove any existing regular file/symlink at the path.
	_ = deps.FS.Remove(maskPath)
	if err := deps.FS.Symlink("/dev/null", maskPath); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("mask-kdump: symlink failed", "err", err)
		}
		return
	}
	// Also remove kdump from any .wants dir so it's not started.
	wantsPaths := []string{
		filepath.Join(mntRoot, "etc", "systemd", "system", "multi-user.target.wants", "kdump.service"),
		filepath.Join(mntRoot, "etc", "systemd", "system", "sysinit.target.wants", "kdump.service"),
	}
	for _, p := range wantsPaths {
		_ = deps.FS.Remove(p)
	}
	if deps.Logger != nil {
		deps.Logger.Info("mask-kdump: kdump.service masked")
	}
}

// normalizeESPLayout ensures the ESP's EFI tree sits at the level the firmware
// expects. The UEFI NVRAM loader path \EFI\<bootID>\<loader> is resolved
// relative to the ESP root: the firmware walks ESP-root/EFI/<bootID>/<loader>.
// FAT32 is case-insensitive, so ESP-root/EFI matches ESP-root/efi — but the
// *level count* must still match. Some cloud images (openEuler 20.03) ship
// their tree one level too deep, at ESP-root/efi/EFI/<bootID>/, which the
// firmware cannot reach (it looks for <bootID> directly under the first-level
// EFI/efi dir, not under a second EFI). The result is "Boot Failed" even
// though every file is present and the NVRAM entry is correct.
//
// This function copies the bootloader files from the nested efi/EFI/<bootID>/
// up to efi/<bootID>/ (which is what the firmware's \EFI\<bootID>\ resolves
// to on a case-insensitive FAT32). Idempotent: if the target level already
// has a loader binary, nothing happens. Best-effort: failures are logged but
// do not abort the install — the image may genuinely not need this fix.
func normalizeESPLayout(ctx context.Context, deps Deps, espMount, bootID string) {
	if espMount == "" || bootID == "" {
		return
	}
	// On FAT32, "EFI" and "efi" resolve to the same directory. The firmware
	// path \EFI\<bootID>\ therefore matches espMount/efi/<bootID>/ — that is
	// the "target" level we want a loader at.
	targetDir := filepath.Join(espMount, "EFI", bootID)
	sourceDir := filepath.Join(espMount, "efi", "EFI", bootID)

	hasLoader := func(dir string) bool {
		for _, loader := range []string{"grubx64.efi", "shimx64.efi", "shim.efi"} {
			if deps.FS.Exists(filepath.Join(dir, loader)) {
				return true
			}
		}
		return false
	}

	if hasLoader(targetDir) {
		return
	}
	if !hasLoader(sourceDir) {
		return
	}

	if err := deps.FS.MkdirAll(targetDir, 0o755); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("normalize-esp: mkdir target dir failed", "dir", targetDir, "err", err)
		}
		return
	}
	out, err := deps.Exec.Run(ctx, "cp", "-a", sourceDir+"/.", targetDir+"/")
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("normalize-esp: cp failed", "from", sourceDir, "to", targetDir, "err", err, "out", string(out))
		}
		return
	}
	if deps.Logger != nil {
		deps.Logger.Info("normalize-esp: copied bootloader to firmware-visible level",
			"bootID", bootID, "from", sourceDir, "to", targetDir)
	}
	if deps.Reporter != nil {
		_ = deps.Reporter.Log(ctx, "info",
			fmt.Sprintf("normalized ESP layout: copied efi/EFI/%s/ → EFI/%s/ (firmware path now resolves)", bootID, bootID))
	}

	// Some images also ship a fallback BOOT/ directory at the wrong level.
	// Copy it the same way so \EFI\BOOT\BOOTX64.EFI (the firmware's last
	// resort) also resolves.
	stdBoot := filepath.Join(espMount, "EFI", "BOOT")
	nestedBoot := filepath.Join(espMount, "efi", "EFI", "BOOT")
	if deps.FS.Exists(filepath.Join(nestedBoot, "BOOTX64.EFI")) && !deps.FS.Exists(filepath.Join(stdBoot, "BOOTX64.EFI")) {
		_ = deps.FS.MkdirAll(stdBoot, 0o755)
		if _, err := deps.Exec.Run(ctx, "cp", "-a", nestedBoot+"/.", stdBoot+"/"); err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("normalize-esp: cp BOOT fallback failed", "err", err)
			}
		}
	}
}
