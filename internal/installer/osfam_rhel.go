// osfam_rhel.go is the OSInstaller for the "rhel" family — covers
// Rocky Linux / AlmaLinux / RHEL 8+ / Fedora. Modern RHEL family uses
// NetworkManager keyfiles for network config and chroot grub2-install
// (UEFI uses pre-baked shim + NVRAM registration). Cloud images ship
// SELinux enforcing → restorecon gate active; installer later disables
// SELinux + masks kdump + rewrites cmdline (all inside
// installGRUBChrootRHEL).
package installer

import (
	"context"

	"metalkit/internal/jobs"
)

type rhelInstaller struct{}

// BuildSeed renders cloud-init NoCloud seed with NetworkManager
// renderer. rhel7=false (modern RHEL uses predictable NIC names).
// isRHELFamily=true → restorecon runs if SELinux is currently enforcing.
func (rhelInstaller) BuildSeed(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot string) error {
	renderer := resolveNetworkRendererFor(spec, "rhel")
	return buildSeedCommon(ctx, deps, spec, mntRoot, renderer, false /* rhel7 */, true /* isRHELFamily */)
}

// InstallBootloader delegates to installGRUBDispatch with the rhel
// family default (grub-chroot-rhel) unless spec.Bootloader overrides.
func (rhelInstaller) InstallBootloader(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath, espMount string) error {
	return installGRUBDispatch(ctx, deps, spec, mntRoot, devPath, espMount, resolveBootloaderFor(spec, "rhel"))
}
