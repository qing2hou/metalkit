// osfam_rhel7.go is the OSInstaller for the "rhel7" family — covers
// CentOS 7 / RHEL 7. CentOS 7's cloud-init 19.4 cannot process
// network-config v2 properly, so we write ifcfg files directly
// (sysconfig renderer). CentOS 7 boots with net.ifnames=0, so netplan
// keys must be eth<N> not live NIC names → rhel7=true. Host grub-install
// (grub-host-fallback) — CentOS 7 has grub2-install but is intentionally
// kept on the host path because that's the known-working production path.
package installer

import (
	"context"

	"metalkit/internal/jobs"
)

type rhel7Installer struct{}

// BuildSeed renders cloud-init NoCloud seed with sysconfig renderer.
// rhel7=true (CentOS 7 net.ifnames=0 → eth<N> naming).
// isRHELFamily=true → restorecon runs if SELinux is currently enforcing.
func (rhel7Installer) BuildSeed(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot string) error {
	renderer := resolveNetworkRendererFor(spec, "rhel7")
	return buildSeedCommon(ctx, deps, spec, mntRoot, renderer, true /* rhel7 */, true /* isRHELFamily */)
}

// InstallBootloader delegates to installGRUBDispatch with the rhel7
// family default (grub-host-fallback) unless spec.Bootloader overrides.
func (rhel7Installer) InstallBootloader(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot, devPath, espMount string) error {
	return installGRUBDispatch(ctx, deps, spec, mntRoot, devPath, espMount, resolveBootloaderFor(spec, "rhel7"))
}
