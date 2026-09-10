package installer

import (
	"testing"

	"metalkit/internal/jobs"
)

func TestArchFromSpec(t *testing.T) {
	// arm64 spec maps to the arm profile across every knob.
	a := archFromSpec(jobs.InstallSpec{ImageArch: "arm64"})
	if !a.IsArm || a.EFIType != "arm64-efi" || a.FallbackFile != "BOOTAA64.EFI" {
		t.Errorf("arm64 profile wrong: %+v", a)
	}
	if a.DebianMetaKernel != "linux-image-arm64" {
		t.Errorf("arm64 kernel = %q", a.DebianMetaKernel)
	}
	if len(a.Loaders) != 2 || a.Loaders[0] != "shimaa64.efi" {
		t.Errorf("arm64 loaders = %v", a.Loaders)
	}

	// amd64 and legacy (empty) both fall back to the x86 profile —
	// back-compat for images uploaded before the arch column existed.
	for _, arch := range []string{"amd64", ""} {
		x := archFromSpec(jobs.InstallSpec{ImageArch: arch})
		if x.IsArm || x.EFIType != "x86_64-efi" || x.FallbackFile != "BOOTX64.EFI" {
			t.Errorf("arch %q profile wrong: %+v", arch, x)
		}
		if x.DebianMetaKernel != "linux-image-amd64" {
			t.Errorf("arch %q kernel = %q", arch, x.DebianMetaKernel)
		}
	}
}
