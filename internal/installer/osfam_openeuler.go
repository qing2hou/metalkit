// osfam_openeuler.go is the OSInstaller for the "openeuler" family.
// openEuler 20.03 / 22.03 / 24.03 LTS: NetworkManager renderer, chroot
// grub2-install. openEuler's quirks (ESP mounted at /boot instead of
// /boot/efi; nested efi/EFI/<id>/ directory layout) are handled inside
// installGRUBChrootRHEL (grub.go: filepath.Rel for chrootEfiDir,
// normalizeESPLayout for the nested fix). SELinux enforcing → restorecon
// gate active.
package installer

import (
	"context"

	"metalkit/internal/jobs"
)

type openEulerInstaller struct{}

// BuildSeed renders cloud-init NoCloud seed with NetworkManager
// renderer. rhel7=false, isRHELFamily=true → restorecon runs if SELinux
// is currently enforcing.
func (openEulerInstaller) BuildSeed(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot string) error {
	renderer := resolveNetworkRendererFor(spec, "openeuler")
	return buildSeedCommon(ctx, deps, spec, mntRoot, renderer, false /* rhel7 */, true /* isRHELFamily */)
}

// InstallBootloader delegates to installGRUBDispatch with the openeuler
// family default (grub-chroot-rhel) unless spec.Bootloader overrides.
func (openEulerInstaller) InstallBootloader(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath, espMount string) error {
	return installGRUBDispatch(ctx, deps, spec, mntRoot, devPath, espMount, resolveBootloaderFor(spec, "openeuler"))
}
