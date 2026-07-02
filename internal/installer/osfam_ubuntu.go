// osfam_ubuntu.go is the OSInstaller for the "ubuntu" family.
// Ubuntu cloudimg ships with full storage drivers and netplan natively,
// so the seed path is plain netplan and the bootloader path is host
// grub-install (Debian-style). No SELinux, no kdump mask, no initramfs
// rebuild needed.
package installer

import (
	"context"

	"metalkit/internal/jobs"
)

type ubuntuInstaller struct{}

// BuildSeed renders cloud-init NoCloud seed with netplan renderer.
// rhel7=false (Ubuntu uses predictable network interface names) and
// isRHELFamily=false (no SELinux restorecon needed).
func (ubuntuInstaller) BuildSeed(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot string) error {
	renderer := resolveNetworkRendererFor(spec, "ubuntu")
	return buildSeedCommon(ctx, deps, spec, mntRoot, renderer, false /* rhel7 */, false /* isRHELFamily */)
}

// InstallBootloader delegates to installGRUBDispatch with the ubuntu
// family default (grub-host-debian) unless spec.Bootloader overrides.
func (ubuntuInstaller) InstallBootloader(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath, espMount string) error {
	return installGRUBDispatch(ctx, deps, spec, mntRoot, devPath, espMount, resolveBootloaderFor(spec, "ubuntu"))
}
