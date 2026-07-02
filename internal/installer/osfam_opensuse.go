// osfam_opensuse.go is the OSInstaller for the "opensuse" family —
// covers openSUSE Leap and Tumbleweed. openSUSE uses wicked as its
// network renderer (writing /etc/sysconfig/network/ifcfg-*). Chroot
// grub2-install for the bootloader. SELinux is not enforcing by default
// on openSUSE but isRHELFamily=true keeps the restorecon gate active as
// a safety net (openSUSE ships SELinux policies even when disabled).
package installer

import (
	"context"

	"metalkit/internal/jobs"
)

type opensuseInstaller struct{}

// BuildSeed renders cloud-init NoCloud seed with wicked renderer.
// rhel7=false, isRHELFamily=true → restorecon runs if SELinux is
// currently enforcing.
func (opensuseInstaller) BuildSeed(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot string) error {
	renderer := resolveNetworkRendererFor(spec, "opensuse")
	return buildSeedCommon(ctx, deps, spec, mntRoot, renderer, false /* rhel7 */, true /* isRHELFamily */)
}

// InstallBootloader delegates to installGRUBDispatch with the opensuse
// family default (grub-chroot-rhel) unless spec.Bootloader overrides.
func (opensuseInstaller) InstallBootloader(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath, espMount string) error {
	return installGRUBDispatch(ctx, deps, spec, mntRoot, devPath, espMount, resolveBootloaderFor(spec, "opensuse"))
}
