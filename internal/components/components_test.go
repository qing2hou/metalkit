package components

import (
	"testing"
)

func TestRenderersForOS(t *testing.T) {
	tests := []struct {
		family       string
		wantDefault  string
		minRenderers int
	}{
		{"ubuntu", "netplan", 2},
		{"debian", "netplan", 2},
		{"rhel", "network-manager", 3},
		{"rhel7", "sysconfig", 2},
		{"kylin", "netplan", 3},
		{"openeuler", "network-manager", 3},
		{"opensuse", "wicked", 3},
		{"any", "netplan", 5},
		{"unknown", "netplan", 5},  // falls back to "any"
		{"", "netplan", 5},         // falls back to "any"
	}

	for _, tt := range tests {
		t.Run(tt.family, func(t *testing.T) {
			opts := RenderersForOS(tt.family)
			if len(opts) < tt.minRenderers {
				t.Errorf("RenderersForOS(%q): got %d options, want at least %d", tt.family, len(opts), tt.minRenderers)
			}
			if opts[0].ID != tt.wantDefault {
				t.Errorf("RenderersForOS(%q): default = %q, want %q", tt.family, opts[0].ID, tt.wantDefault)
			}
			// Verify all options have non-empty labels and descriptions.
			for _, o := range opts {
				if o.ID == "" {
					t.Errorf("RenderersForOS(%q): empty ID in option", tt.family)
				}
				if o.Label == "" {
					t.Errorf("RenderersForOS(%q): empty Label for %q", tt.family, o.ID)
				}
			}
		})
	}
}

func TestBootloadersForOS(t *testing.T) {
	tests := []struct {
		family       string
		wantDefault  string
		minBoot      int
	}{
		{"ubuntu", "grub-host-debian", 2},
		{"debian", "grub-host-debian", 2},
		{"rhel", "grub-chroot-rhel", 2},
		{"rhel7", "grub-host-fallback", 2},
		{"kylin", "grub-host-debian", 2},
		{"openeuler", "grub-chroot-rhel", 2},
		{"opensuse", "grub-chroot-rhel", 2},
		{"any", "grub-host-debian", 3},
	}

	for _, tt := range tests {
		t.Run(tt.family, func(t *testing.T) {
			opts := BootloadersForOS(tt.family)
			if len(opts) < tt.minBoot {
				t.Errorf("BootloadersForOS(%q): got %d options, want at least %d", tt.family, len(opts), tt.minBoot)
			}
			if opts[0].ID != tt.wantDefault {
				t.Errorf("BootloadersForOS(%q): default = %q, want %q", tt.family, opts[0].ID, tt.wantDefault)
			}
		})
	}
}

func TestDefaultRenderer(t *testing.T) {
	if DefaultRenderer("rhel") != NetworkManagerRenderer {
		t.Errorf("DefaultRenderer(rhel) = %q, want %q", DefaultRenderer("rhel"), NetworkManagerRenderer)
	}
	if DefaultRenderer("ubuntu") != NetPlanRenderer {
		t.Errorf("DefaultRenderer(ubuntu) = %q, want %q", DefaultRenderer("ubuntu"), NetPlanRenderer)
	}
	if DefaultRenderer("opensuse") != WickedRenderer {
		t.Errorf("DefaultRenderer(opensuse) = %q, want %q", DefaultRenderer("opensuse"), WickedRenderer)
	}
}

func TestDefaultBootloader(t *testing.T) {
	if DefaultBootloader("rhel") != GRUBChrootRHEL {
		t.Errorf("DefaultBootloader(rhel) = %q, want %q", DefaultBootloader("rhel"), GRUBChrootRHEL)
	}
	if DefaultBootloader("ubuntu") != GRUBHostDebian {
		t.Errorf("DefaultBootloader(ubuntu) = %q, want %q", DefaultBootloader("ubuntu"), GRUBHostDebian)
	}
}

func TestValidateNetworkRenderer(t *testing.T) {
	valid := []string{"", "netplan", "network-manager", "sysconfig", "eni", "wicked"}
	for _, v := range valid {
		if err := ValidateNetworkRenderer(v); err != nil {
			t.Errorf("ValidateNetworkRenderer(%q): unexpected error: %v", v, err)
		}
	}
	invalid := []string{"unknown", "Netplan", "NETWORK-MANAGER", "ifcfg"}
	for _, v := range invalid {
		if err := ValidateNetworkRenderer(v); err == nil {
			t.Errorf("ValidateNetworkRenderer(%q): expected error, got nil", v)
		}
	}
}

func TestValidateBootloader(t *testing.T) {
	valid := []string{"", "grub-host-debian", "grub-chroot-rhel", "grub-host-fallback"}
	for _, v := range valid {
		if err := ValidateBootloader(v); err != nil {
			t.Errorf("ValidateBootloader(%q): unexpected error: %v", v, err)
		}
	}
	invalid := []string{"unknown", "systemd-boot", "GRUB-Host-Debian"}
	for _, v := range invalid {
		if err := ValidateBootloader(v); err == nil {
			t.Errorf("ValidateBootloader(%q): expected error, got nil", v)
		}
	}
}

func TestComponentsForOS(t *testing.T) {
	cs := ComponentsForOS("rhel")
	if len(cs.Renderers) == 0 {
		t.Error("ComponentsForOS(rhel): no renderers")
	}
	if len(cs.Bootloaders) == 0 {
		t.Error("ComponentsForOS(rhel): no bootloaders")
	}
	if cs.Renderers[0].ID != "network-manager" {
		t.Errorf("ComponentsForOS(rhel): default renderer = %q, want network-manager", cs.Renderers[0].ID)
	}
}

func TestKnownOSFamilies(t *testing.T) {
	families := KnownOSFamilies()
	if len(families) < 7 {
		t.Errorf("KnownOSFamilies(): got %d families, want at least 7", len(families))
	}
	// Should not include "any".
	for _, f := range families {
		if f == "any" {
			t.Error("KnownOSFamilies(): should not include 'any'")
		}
	}
}

func TestAllRendererOptions(t *testing.T) {
	opts := AllRendererOptions()
	if len(opts) < 5 {
		t.Errorf("AllRendererOptions(): got %d, want at least 5", len(opts))
	}
}

func TestAllBootloaderOptions(t *testing.T) {
	opts := AllBootloaderOptions()
	if len(opts) < 3 {
		t.Errorf("AllBootloaderOptions(): got %d, want at least 3", len(opts))
	}
}
