// osfam_debian.go is the OSInstaller for the "debian" family.
// Debian generic/genericcloud images: netplan renderer (when cloud-init
// present) or direct netplan write, host grub-install. The cloud kernel
// on Debian 13/14 strips RAID drivers — that's handled inside
// installGRUBHostDebian → regenerateInitramfsDebian, not here.
package installer

import (
	"context"

	"metalkit/internal/jobs"
)

type debianInstaller struct{}

// BuildSeed renders cloud-init NoCloud seed with netplan renderer.
// rhel7=false, isRHELFamily=false (no SELinux on Debian).
func (debianInstaller) BuildSeed(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot string) error {
	renderer := resolveNetworkRendererFor(spec, "debian")
	return buildSeedCommon(ctx, deps, spec, mntRoot, renderer, false /* rhel7 */, false /* isRHELFamily */)
}

// InstallBootloader delegates to installGRUBDispatch with the debian
// family default (grub-host-debian) unless spec.Bootloader overrides.
func (debianInstaller) InstallBootloader(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath, espMount string) error {
	return installGRUBDispatch(ctx, deps, spec, mntRoot, devPath, espMount, resolveBootloaderFor(spec, "debian"))
}
