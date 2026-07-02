// osfam_kylin.go is the OSInstaller for the "kylin" family — covers
// 银河麒麟 (Kylin) V10 (Ubuntu-based) and V4/Server (CentOS-based).
// Default path matches Ubuntu (netplan + host grub-install). Operators
// deploying the CentOS-based variant can override via spec.Bootloader =
// "grub-chroot-rhel" in the profile — installGRUBDispatch honors the
// explicit selection. No SELinux on Kylin desktop variants; Server
// variants that ship SELinux will be handled by the RHEL-family gate
// inside buildSeedCommon (isRHELFamily=false here means restorecon is
// skipped — Kylin Server images typically pre-disable SELinux).
package installer

import (
	"context"

	"metalkit/internal/jobs"
)

type kylinInstaller struct{}

// BuildSeed renders cloud-init NoCloud seed with netplan renderer.
// rhel7=false, isRHELFamily=false.
func (kylinInstaller) BuildSeed(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot string) error {
	renderer := resolveNetworkRendererFor(spec, "kylin")
	return buildSeedCommon(ctx, deps, spec, mntRoot, renderer, false /* rhel7 */, false /* isRHELFamily */)
}

// InstallBootloader delegates to installGRUBDispatch with the kylin
// family default (grub-host-debian) unless spec.Bootloader overrides.
func (kylinInstaller) InstallBootloader(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath, espMount string) error {
	return installGRUBDispatch(ctx, deps, spec, mntRoot, devPath, espMount, resolveBootloaderFor(spec, "kylin"))
}
