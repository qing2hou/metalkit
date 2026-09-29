package installer

import (
	"metalkit/internal/jobs"
)

// Arch selection: image arch describes the CPU the disk image targets, which
// is what the bootloader paths and grub-install targets must match. We read
// it from the InstallSpec; the fallback for legacy images (arch unknown) and
// for amd64 tools run on an amd64 live host is x86 — preserving the
// pre-multi-arch behaviour exactly.
type archInfo struct {
	IsArm   bool
	EFIType string // grub-install --target value for UEFI
	// EFI loaders, most-preferred first (NVRAM registration + ESP probing).
	Loaders []string
	// FallbackFileName is the ESP bootloader the firmware looks for by
	// default: \EFI\BOOT\BOOTX64.EFI on x86, BOOTAA64.EFI on arm64.
	FallbackFile string
	// DebianMetaKernel is the distro metapackage that pulls a standard
	// kernel of the right flavour (installed when a cloud image ships none).
	DebianMetaKernel string
	// SerialConsoles lists the console= devices to stage for the
	// architecture: x86 servers use 8250-style ttyS0; arm64 servers'
	// BMCs typically expose a PL011 UART (ttyAMA0).
	SerialConsoles []string
}

// archFromSpec normalises spec.ImageArch into archInfo. Unknown/empty arch
// maps to the amd64 profile (back-compat).
func archFromSpec(spec jobs.InstallSpec) archInfo {
	switch spec.ImageArch {
	case "arm64":
		return archInfo{
			IsArm:            true,
			EFIType:          "arm64-efi",
			Loaders:          []string{"shimaa64.efi", "grubaa64.efi"},
			FallbackFile:     "BOOTAA64.EFI",
			DebianMetaKernel: "linux-image-arm64",
			SerialConsoles:   []string{"ttyAMA0", "ttyS0"},
		}
	default: // "" (legacy image) and "amd64"
		return archInfo{
			IsArm:            false,
			EFIType:          "x86_64-efi",
			Loaders:          []string{"shimx64.efi", "shim.efi", "grubx64.efi"},
			FallbackFile:     "BOOTX64.EFI",
			DebianMetaKernel: "linux-image-amd64",
			SerialConsoles:   []string{"ttyS0"},
		}
	}
}
