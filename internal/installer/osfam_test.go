// osfam_test.go verifies the per-OS-family dispatch surface:
// pickOSInstaller returns the right OSInstaller implementation for each
// family, and rejects empty / "any" / unknown families.
package installer

import (
	"reflect"
	"testing"

	"metalkit/internal/components"
	"metalkit/internal/jobs"
	"metalkit/internal/profiles"
)

// typeName returns the concrete type name of an OSInstaller implementation,
// stripped of package prefix (e.g. "installer.ubuntuInstaller" → "ubuntuInstaller").
func typeName(inst OSInstaller) string {
	return reflect.TypeOf(inst).Name()
}

// specForFamily builds a minimal InstallSpec with the given OSFamily set
// on the Profile. Used by tests that need to exercise resolve*For helpers
// without constructing a full spec.
func specForFamily(family string) jobs.InstallSpec {
	return jobs.InstallSpec{
		Profile: profiles.Profile{OSFamily: family},
	}
}

func TestPickOSInstaller_KnownFamilies(t *testing.T) {
	cases := []struct {
		family string
		want   string // type name suffix
	}{
		{"ubuntu", "ubuntuInstaller"},
		{"debian", "debianInstaller"},
		{"rhel", "rhelInstaller"},
		{"rhel7", "rhel7Installer"},
		{"kylin", "kylinInstaller"},
		{"openeuler", "openEulerInstaller"},
		{"opensuse", "opensuseInstaller"},
	}
	for _, tc := range cases {
		t.Run(tc.family, func(t *testing.T) {
			inst, err := pickOSInstaller(tc.family)
			if err != nil {
				t.Fatalf("pickOSInstaller(%q) error: %v", tc.family, err)
			}
			got := typeName(inst)
			if got != tc.want {
				t.Errorf("pickOSInstaller(%q) = %s, want %s", tc.family, got, tc.want)
			}
		})
	}
}

func TestPickOSInstaller_RejectsEmptyAndUnknown(t *testing.T) {
	for _, fam := range []string{"", "any", "windows", "macos", "UBUNTU"} {
		t.Run(fam, func(t *testing.T) {
			inst, err := pickOSInstaller(fam)
			if err == nil {
				t.Errorf("pickOSInstaller(%q) = %v, want error", fam, inst)
			}
		})
	}
}

// TestResolveBootloaderFor_Defaults verifies each family resolves to the
// bootloader strategy documented in components.go's bootloaderMap.
func TestResolveBootloaderFor_Defaults(t *testing.T) {
	cases := []struct {
		family string
		want   components.Bootloader
	}{
		{"ubuntu", components.GRUBHostDebian},
		{"debian", components.GRUBHostDebian},
		{"rhel", components.GRUBChrootRHEL},
		{"rhel7", components.GRUBHostFallback},
		{"kylin", components.GRUBHostDebian},
		{"openeuler", components.GRUBChrootRHEL},
		{"opensuse", components.GRUBChrootRHEL},
	}
	for _, tc := range cases {
		t.Run(tc.family, func(t *testing.T) {
			got := resolveBootloaderFor(specForFamily(tc.family), tc.family)
			if got != tc.want {
				t.Errorf("resolveBootloaderFor(family=%q) = %q, want %q", tc.family, got, tc.want)
			}
		})
	}
}

// TestResolveBootloaderFor_ExplicitOverride verifies spec.Bootloader wins
// over the family default.
func TestResolveBootloaderFor_ExplicitOverride(t *testing.T) {
	spec := specForFamily("ubuntu")
	spec.Bootloader = "grub-chroot-rhel"
	got := resolveBootloaderFor(spec, "ubuntu")
	if got != components.GRUBChrootRHEL {
		t.Errorf("explicit override ignored: got %q, want grub-chroot-rhel", got)
	}
}

// TestResolveNetworkRendererFor_Defaults verifies each family resolves to
// the network renderer documented in components.go's rendererMap.
func TestResolveNetworkRendererFor_Defaults(t *testing.T) {
	cases := []struct {
		family string
		want   components.NetworkRenderer
	}{
		{"ubuntu", components.NetPlanRenderer},
		{"debian", components.NetPlanRenderer},
		{"rhel", components.NetworkManagerRenderer},
		{"rhel7", components.SysconfigRenderer},
		{"kylin", components.NetPlanRenderer},
		{"openeuler", components.NetworkManagerRenderer},
		{"opensuse", components.WickedRenderer},
	}
	for _, tc := range cases {
		t.Run(tc.family, func(t *testing.T) {
			got := resolveNetworkRendererFor(specForFamily(tc.family), tc.family)
			if got != tc.want {
				t.Errorf("resolveNetworkRendererFor(family=%q) = %q, want %q", tc.family, got, tc.want)
			}
		})
	}
}
