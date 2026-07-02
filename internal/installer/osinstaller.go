// osinstaller.go defines the per-OS-family dispatch surface. Each OS
// family (ubuntu / debian / rhel / rhel7 / kylin / openeuler / opensuse)
// has a dedicated OSInstaller implementation in osfam_<family>.go. The
// installer pipeline (installer.go:Run) picks the implementation once
// from spec.Profile.OSFamily and delegates the two OS-sensitive stages
// (seed, grub-install) to it. All other stages are OS-agnostic and stay
// in installer.go.
//
// Adding a new OS family is purely additive: drop a new osfam_<family>.go
// file implementing OSInstaller, add one line to osInstallers, add the
// family string to profiles.validOSFamilies. No existing per-OS file is
// touched.
//
// spec.Profile.OSFamily is authoritative — the installer no longer
// re-detects the family from /etc/os-release at runtime. Profile
// metadata must match the deployed image (see IMAGE-PREP.md).
package installer

import (
	"context"
	"fmt"

	"metalkit/internal/components"
	"metalkit/internal/jobs"
)

// OSInstaller encapsulates the OS-sensitive stages of the install
// pipeline for one OS family. Methods must be safe to call in sequence
// (BuildSeed first, then InstallBootloader) on the same instance.
type OSInstaller interface {
	// BuildSeed renders cloud-init NoCloud seed (user-data, meta-data,
	// network-config) into <mntRoot>/var/lib/cloud/seed/nocloud-net/ and
	// applies any renderer-specific post-processing (NM keyfiles, ifcfg,
	// wicked, etc.). Also handles SELinux restorecon gating for RHEL
	// family and direct-write fallback when cloud-init is absent.
	BuildSeed(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot string) error

	// InstallBootloader installs GRUB onto the target disk and
	// regenerates grub.cfg + initramfs as needed for the family's
	// bootloader strategy (host-grub vs chroot-grub2). Also applies
	// family-specific post-install fixes (SELinux disable, kdump mask,
	// cmdline rewrite, kernel pinning).
	InstallBootloader(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath, espMount string) error
}

// osInstallers is the closed registry of OS family → implementation.
// Empty string and "any" are intentionally absent — pickOSInstaller
// returns an error for them, forcing the operator to pick a concrete
// family in the profile.
var osInstallers = map[string]OSInstaller{
	"ubuntu":    ubuntuInstaller{},
	"debian":    debianInstaller{},
	"rhel":      rhelInstaller{},
	"rhel7":     rhel7Installer{},
	"kylin":     kylinInstaller{},
	"openeuler": openEulerInstaller{},
	"opensuse":  opensuseInstaller{},
}

// pickOSInstaller returns the OSInstaller for the given family string.
// Returns an error for empty / "any" / unknown families — the pipeline
// fails fast rather than silently falling back, because a wrong family
// selection produces an unbootable disk.
func pickOSInstaller(family string) (OSInstaller, error) {
	if inst, ok := osInstallers[family]; ok {
		return inst, nil
	}
	return nil, fmt.Errorf(
		"os_family %q not in ubuntu/debian/rhel/rhel7/kylin/openeuler/opensuse — set a concrete family in the profile",
		family,
	)
}

// resolveNetworkRendererFor returns the effective network renderer:
// explicit spec.NetworkRenderer wins, else the family's default from
// the components registry.
func resolveNetworkRendererFor(spec jobs.InstallSpec, family string) components.NetworkRenderer {
	if r := components.NetworkRenderer(spec.NetworkRenderer); r != "" {
		return r
	}
	return components.DefaultRenderer(family)
}

// resolveBootloaderFor returns the effective bootloader strategy:
// explicit spec.Bootloader wins, else the family's default from the
// components registry.
func resolveBootloaderFor(spec jobs.InstallSpec, family string) components.Bootloader {
	if b := components.Bootloader(spec.Bootloader); b != "" {
		return b
	}
	return components.DefaultBootloader(family)
}
