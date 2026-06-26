// Package components defines the install-component registry shared between
// the profiles API (for validation and UI option lists) and the installer
// pipeline (for renderer/bootloader dispatch). Placing these types here
// avoids circular imports between internal/profiles and internal/installer.
//
// Adding a new OS family requires three steps:
//
//   1. Add the family to the rendererMap/bootloaderMap below.
//   2. Add filename-detection rules in internal/images/detect.go.
//   3. Register the family in internal/profiles/validate.go validOSFamilies.
//
// No database schema changes are needed — the profile's network_renderer
// and bootloader columns are free-form TEXT validated against this registry.
package components

import (
	"fmt"
	"strings"
)

// --- Network renderers -------------------------------------------------------

// NetworkRenderer identifies a network-configuration rendering strategy.
type NetworkRenderer string

const (
	// NetPlanRenderer emits cloud-init Network Config v2 for netplan
	// (Ubuntu/Debian default). Cloud-init renders /etc/netplan/50-cloud-init.yaml.
	NetPlanRenderer NetworkRenderer = "netplan"

	// NetworkManagerRenderer emits cloud-init v2 with NM renderer override
	// (modern RHEL/openEuler default). Produces .nmconnection keyfiles.
	NetworkManagerRenderer NetworkRenderer = "network-manager"

	// SysconfigRenderer writes ifcfg files directly
	// (RHEL7 / CentOS 7 fallback). Bypasses cloud-init network-config v2.
	SysconfigRenderer NetworkRenderer = "sysconfig"

	// ENIRenderer writes /etc/network/interfaces directly
	// (Debian legacy, pre-netplan). Also sets cloud-init eni renderer priority.
	ENIRenderer NetworkRenderer = "eni"

	// WickedRenderer writes openSUSE Wicked ifcfg files directly
	// (/etc/sysconfig/network/ifcfg-*). openSUSE default.
	WickedRenderer NetworkRenderer = "wicked"
)

// --- Bootloaders -------------------------------------------------------------

// Bootloader identifies a bootloader installation strategy.
type Bootloader string

const (
	// GRUBHostDebian uses the host's grub-install + chroot update-grub
	// (Ubuntu/Debian/Kylin default).
	GRUBHostDebian Bootloader = "grub-host-debian"

	// GRUBChrootRHEL uses chroot grub2-install + grub2-mkconfig
	// (modern RHEL/openEuler/openSUSE default).
	GRUBChrootRHEL Bootloader = "grub-chroot-rhel"

	// GRUBHostFallback uses host grub-install with multi-tier update-grub
	// fallback chain (safe fallback for any distro).
	GRUBHostFallback Bootloader = "grub-host-fallback"
)

// --- UI-facing types ---------------------------------------------------------

// ComponentOption describes one selectable component for the UI dropdown.
type ComponentOption struct {
	ID          string `json:"id"`          // e.g. "netplan", "grub-host-debian"
	Label       string `json:"label"`       // e.g. "Netplan (cloud-init v2)"
	Description string `json:"description"` // e.g. "Ubuntu/Debian 默认，cloud-init 渲染 /etc/netplan/"
}

// ComponentSet groups the available renderers and bootloaders for one OS family.
type ComponentSet struct {
	Renderers   []ComponentOption `json:"renderers"`
	Bootloaders []ComponentOption `json:"bootloaders"`
}

// --- Registry data -----------------------------------------------------------

// rendererMap maps each OS family to its ordered list of available network
// renderers. The first entry is the default. The "any" key lists all known
// renderers for profiles that are OS-agnostic.
var rendererMap = map[string][]ComponentOption{
	"ubuntu": {
		{ID: "netplan", Label: "Netplan (cloud-init v2)", Description: "Ubuntu/Debian 默认，cloud-init 渲染 /etc/netplan/"},
		{ID: "eni", Label: "ENI (/etc/network/interfaces)", Description: "Debian 旧式 ifupdown，适用于不使用 netplan 的系统"},
	},
	"debian": {
		{ID: "netplan", Label: "Netplan (cloud-init v2)", Description: "Ubuntu/Debian 默认，cloud-init 渲染 /etc/netplan/"},
		{ID: "eni", Label: "ENI (/etc/network/interfaces)", Description: "Debian 旧式 ifupdown，适用于不使用 netplan 的系统"},
	},
	"rhel": {
		{ID: "network-manager", Label: "NetworkManager (keyfiles)", Description: "现代 RHEL 默认，cloud-init 渲染 .nmconnection"},
		{ID: "netplan", Label: "Netplan (cloud-init v2)", Description: "RHEL 可选，需安装 netplan.io 包"},
		{ID: "sysconfig", Label: "Sysconfig (ifcfg)", Description: "RHEL7 旧式 /etc/sysconfig/network-scripts/ifcfg-*"},
	},
	"rhel7": {
		{ID: "sysconfig", Label: "Sysconfig (ifcfg)", Description: "CentOS 7 / RHEL 7 默认，cloud-init 19.4 兼容"},
		{ID: "netplan", Label: "Netplan (cloud-init v2)", Description: "RHEL7 可选（需额外安装 netplan）"},
	},
	"kylin": {
		{ID: "netplan", Label: "Netplan (cloud-init v2)", Description: "银河麒麟 V10 默认（基于 Ubuntu）"},
		{ID: "network-manager", Label: "NetworkManager (keyfiles)", Description: "麒麟 V4/Server 默认（基于 CentOS）"},
		{ID: "eni", Label: "ENI (/etc/network/interfaces)", Description: "旧式 ifupdown 兼容"},
	},
	"openeuler": {
		{ID: "network-manager", Label: "NetworkManager (keyfiles)", Description: "openEuler 默认，cloud-init 渲染 .nmconnection"},
		{ID: "netplan", Label: "Netplan (cloud-init v2)", Description: "openEuler 可选"},
		{ID: "sysconfig", Label: "Sysconfig (ifcfg)", Description: "openEuler 旧版兼容"},
	},
	"opensuse": {
		{ID: "wicked", Label: "Wicked (ifcfg)", Description: "openSUSE 默认，/etc/sysconfig/network/ifcfg-*"},
		{ID: "network-manager", Label: "NetworkManager (keyfiles)", Description: "openSUSE 可选，适用于桌面/混合场景"},
		{ID: "netplan", Label: "Netplan (cloud-init v2)", Description: "openSUSE 可选（需安装 netplan）"},
	},
	"any": {
		{ID: "netplan", Label: "Netplan (cloud-init v2)", Description: "Ubuntu/Debian 默认"},
		{ID: "network-manager", Label: "NetworkManager (keyfiles)", Description: "RHEL/openEuler 默认"},
		{ID: "eni", Label: "ENI (/etc/network/interfaces)", Description: "Debian 旧式"},
		{ID: "sysconfig", Label: "Sysconfig (ifcfg)", Description: "RHEL7/CentOS 7 旧式"},
		{ID: "wicked", Label: "Wicked (ifcfg)", Description: "openSUSE 默认"},
	},
}

// bootloaderMap maps each OS family to its ordered list of available bootloaders.
var bootloaderMap = map[string][]ComponentOption{
	"ubuntu": {
		{ID: "grub-host-debian", Label: "GRUB (host + update-grub)", Description: "Ubuntu/Debian 默认，host grub-install + chroot update-grub"},
		{ID: "grub-host-fallback", Label: "GRUB (host fallback)", Description: "通用 fallback，host grub-install + 多级 mkconfig 回退"},
	},
	"debian": {
		{ID: "grub-host-debian", Label: "GRUB (host + update-grub)", Description: "Ubuntu/Debian 默认，host grub-install + chroot update-grub"},
		{ID: "grub-host-fallback", Label: "GRUB (host fallback)", Description: "通用 fallback，host grub-install + 多级 mkconfig 回退"},
	},
	"rhel": {
		{ID: "grub-chroot-rhel", Label: "GRUB2 (chroot)", Description: "RHEL 默认，chroot grub2-install + grub2-mkconfig"},
		{ID: "grub-host-fallback", Label: "GRUB (host fallback)", Description: "通用 fallback，host grub-install + 多级 mkconfig 回退"},
	},
	"rhel7": {
		{ID: "grub-host-fallback", Label: "GRUB (host fallback)", Description: "CentOS 7 默认，host grub-install + 多级 mkconfig 回退"},
		{ID: "grub-chroot-rhel", Label: "GRUB2 (chroot)", Description: "RHEL7 可选，chroot grub2-install"},
	},
	"kylin": {
		{ID: "grub-host-debian", Label: "GRUB (host + update-grub)", Description: "麒麟 V10 默认（基于 Ubuntu）"},
		{ID: "grub-chroot-rhel", Label: "GRUB2 (chroot)", Description: "麒麟 V4/Server 默认（基于 CentOS）"},
	},
	"openeuler": {
		{ID: "grub-chroot-rhel", Label: "GRUB2 (chroot)", Description: "openEuler 默认，chroot grub2-install + grub2-mkconfig"},
		{ID: "grub-host-fallback", Label: "GRUB (host fallback)", Description: "通用 fallback"},
	},
	"opensuse": {
		{ID: "grub-chroot-rhel", Label: "GRUB2 (chroot)", Description: "openSUSE 默认，chroot grub2-install + grub2-mkconfig"},
		{ID: "grub-host-fallback", Label: "GRUB (host fallback)", Description: "通用 fallback"},
	},
	"any": {
		{ID: "grub-host-debian", Label: "GRUB (host + update-grub)", Description: "Ubuntu/Debian 默认"},
		{ID: "grub-chroot-rhel", Label: "GRUB2 (chroot)", Description: "RHEL/openEuler/openSUSE 默认"},
		{ID: "grub-host-fallback", Label: "GRUB (host fallback)", Description: "通用 fallback"},
	},
}

// allRendererIDs is the closed set of valid network_renderer values.
var allRendererIDs = map[string]bool{
	"netplan":          true,
	"network-manager":  true,
	"sysconfig":        true,
	"eni":              true,
	"wicked":           true,
}

// allBootloaderIDs is the closed set of valid bootloader values.
var allBootloaderIDs = map[string]bool{
	"grub-host-debian":  true,
	"grub-chroot-rhel":  true,
	"grub-host-fallback": true,
}

// --- Public query functions --------------------------------------------------

// RenderersForOS returns the available network renderers for the given OS
// family. The first entry is the default. Falls back to the "any" list for
// unknown families.
func RenderersForOS(family string) []ComponentOption {
	family = strings.ToLower(strings.TrimSpace(family))
	if opts, ok := rendererMap[family]; ok {
		return opts
	}
	return rendererMap["any"]
}

// BootloadersForOS returns the available bootloaders for the given OS family.
// Falls back to the "any" list for unknown families.
func BootloadersForOS(family string) []ComponentOption {
	family = strings.ToLower(strings.TrimSpace(family))
	if opts, ok := bootloaderMap[family]; ok {
		return opts
	}
	return bootloaderMap["any"]
}

// DefaultRenderer returns the default network renderer for the given OS family.
func DefaultRenderer(family string) NetworkRenderer {
	opts := RenderersForOS(family)
	if len(opts) > 0 {
		return NetworkRenderer(opts[0].ID)
	}
	return NetPlanRenderer
}

// DefaultBootloader returns the default bootloader for the given OS family.
func DefaultBootloader(family string) Bootloader {
	opts := BootloadersForOS(family)
	if len(opts) > 0 {
		return Bootloader(opts[0].ID)
	}
	return GRUBHostDebian
}

// AllRendererOptions returns all known renderer options (for "any" family).
func AllRendererOptions() []ComponentOption {
	return rendererMap["any"]
}

// AllBootloaderOptions returns all known bootloader options (for "any" family).
func AllBootloaderOptions() []ComponentOption {
	return bootloaderMap["any"]
}

// ComponentsForOS returns the full ComponentSet for a given OS family.
func ComponentsForOS(family string) ComponentSet {
	return ComponentSet{
		Renderers:   RenderersForOS(family),
		Bootloaders: BootloadersForOS(family),
	}
}

// --- Validation functions ----------------------------------------------------

// ValidateNetworkRenderer checks that s is a known renderer or empty.
// Empty string is always valid (means "use default for the OS family").
func ValidateNetworkRenderer(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if !allRendererIDs[s] {
		return fmt.Errorf("network_renderer %q: must be one of netplan/network-manager/sysconfig/eni/wicked", s)
	}
	return nil
}

// ValidateBootloader checks that s is a known bootloader or empty.
// Empty string is always valid (means "use default for the OS family").
func ValidateBootloader(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if !allBootloaderIDs[s] {
		return fmt.Errorf("bootloader %q: must be one of grub-host-debian/grub-chroot-rhel/grub-host-fallback", s)
	}
	return nil
}

// KnownOSFamilies returns all OS family keys that have component mappings.
func KnownOSFamilies() []string {
	families := make([]string, 0, len(rendererMap))
	for k := range rendererMap {
		if k != "any" {
			families = append(families, k)
		}
	}
	return families
}
