// seed.go renders the cloud-init NoCloud seed for the freshly written
// image and stages it inside the rootfs at the standard location
// `/var/lib/cloud/seed/nocloud-net/`. Cloud-init's NoCloud datasource
// auto-discovers seeds under that directory on first boot — no kernel
// cmdline or ISO mount required.
//
// Three files go into the seed:
//
//   - user-data:      chpasswd + ssh_pwauth + hostname + sshd drop-in.
//   - meta-data:      instance-id and local-hostname (NoCloud minimum).
//   - network-config: cloud-init Network Config v2 — cloud-init renders
//                     this into the single /etc/netplan/50-cloud-init.yaml
//                     on first boot. We do NOT write our own netplan file
//                     via user-data write_files: that produced two
//                     conflicting files (01-metalkit.yaml + 50-cloud-init
//                     .yaml) and netplan applied the wrong one. Letting
//                     cloud-init own the only netplan file removes the
//                     conflict.
//
// Network config rendering:
//
//   - Single-NIC: one ethernet device "metalkit0" matched by MAC, name, or
//     wildcard (en*).
//   - Bond: one ethernet per slave interface name. The agent resolves each
//     name to its MAC on the live system (from Deps.NICs) and emits a
//     match.macaddress block for stability. If the NIC list is unavailable
//     (nil/empty), it falls back to match.name.
//   - VLAN: when NetworkConfig.VLAN > 0, the IP config moves onto a VLAN
//     sub-interface (metalkit0.<vlan> or bond0.<vlan>) and the underlying
//     physical/bond device stays L2-only.
//
// We deliberately keep the YAML emission hand-rolled. The fields are
// constrained by the profile/binding validators upstream and a YAML
// library would be a new dependency for one tiny job. The renderer
// quotes string values with %q which produces RFC-compliant JSON-ish
// double quotes; YAML accepts that for scalars.
package installer

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"metalkit/internal/bindings"
	"metalkit/internal/jobs"
	"metalkit/internal/profiles"
)

// BuildSeed renders user-data + meta-data + network-config into
// <mntRoot>/var/lib/cloud/seed/nocloud-net/ so cloud-init on the
// installed system auto-discovers it on first boot.
func BuildSeed(ctx context.Context, deps Deps, spec jobs.InstallSpec, mntRoot string) error {
	if mntRoot == "" {
		return fmt.Errorf("install: BuildSeed: mntRoot is empty")
	}
	seedDir := filepath.Join(mntRoot, "var", "lib", "cloud", "seed", "nocloud-net")
	if err := deps.FS.MkdirAll(seedDir, 0o755); err != nil {
		return fmt.Errorf("install: mkdir seed dir: %w", err)
	}

	host := expandHostname(spec.Profile.HostnameTemplate, spec.Binding, spec.Profile)
	userData := renderUserData(spec, host)
	metaData := renderMetaData(spec, host)
	netCfg := renderNetworkConfig(spec.Profile.Network, spec.Binding, deps.NICs)

	userPath := filepath.Join(seedDir, "user-data")
	metaPath := filepath.Join(seedDir, "meta-data")
	netPath := filepath.Join(seedDir, "network-config")
	if err := deps.FS.WriteFile(userPath, []byte(userData), 0o644); err != nil {
		return fmt.Errorf("install: write user-data: %w", err)
	}
	if err := deps.FS.WriteFile(metaPath, []byte(metaData), 0o644); err != nil {
		return fmt.Errorf("install: write meta-data: %w", err)
	}
	if err := deps.FS.WriteFile(netPath, []byte(netCfg), 0o644); err != nil {
		return fmt.Errorf("install: write network-config: %w", err)
	}
	return nil
}

// renderUserData emits the #cloud-config YAML body. Stable ordering of
// keys + indentation matches the golden test fixtures byte-for-byte.
//
// Ubuntu cloudimg ships with `disable_root: true` and sshd's
// `PermitRootLogin prohibit-password`. Our flow installs a root password
// hash from the profile and expects operators to SSH in as root with
// that password — so we override both: cloud-init `disable_root: false`
// keeps /root/.ssh/authorized_keys writable, and a sshd drop-in
// (`/etc/ssh/sshd_config.d/99-metalkit.conf`) flips PermitRootLogin +
// PasswordAuthentication to `yes`. The runcmd `systemctl restart ssh`
// makes the change take effect on first boot.
//
// Network config is NOT emitted here. It goes into the NoCloud
// `network-config` file (see renderNetworkConfig) so cloud-init owns
// the single /etc/netplan/50-cloud-init.yaml on the installed system.
func renderUserData(spec jobs.InstallSpec, hostname string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	fmt.Fprintf(&b, "hostname: %q\n", hostname)
	b.WriteString("disable_root: false\n")
	b.WriteString("ssh_pwauth: true\n")
	b.WriteString("chpasswd:\n")
	b.WriteString("  expire: false\n")
	b.WriteString("  users:\n")
	b.WriteString("    - name: root\n")
	fmt.Fprintf(&b, "      password: %q\n", spec.Profile.RootPasswordHash)
	b.WriteString("      type: hash\n")

	b.WriteString("write_files:\n")
	b.WriteString("  - path: /etc/ssh/sshd_config.d/99-metalkit.conf\n")
	b.WriteString("    permissions: \"0644\"\n")
	b.WriteString("    owner: root:root\n")
	b.WriteString("    content: |\n")
	b.WriteString("      # Managed by metalkit installer\n")
	b.WriteString("      PermitRootLogin yes\n")
	b.WriteString("      PasswordAuthentication yes\n")

	b.WriteString("runcmd:\n")
	b.WriteString("  - [systemctl, restart, ssh]\n")
	return b.String()
}

// renderNetworkConfig emits a cloud-init Network Config v2 document
// (no outer `network:` wrapper — that's what NoCloud's network-config
// file format expects). cloud-init on first boot reads this and renders
// /etc/netplan/50-cloud-init.yaml accordingly.
//
// nics is the live system's NIC list used to resolve bond slave interface
// names to MACs. May be nil (tests, or agent didn't collect NICs) — in
// that case bond slaves fall back to match.name.
func renderNetworkConfig(nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo) string {
	if nc.Bond != nil {
		return renderNetworkConfigBond(nc, b, nics)
	}
	return renderNetworkConfigSingle(nc, b)
}

func renderNetworkConfigSingle(nc profiles.NetworkConfig, b bindings.Binding) string {
	var sb strings.Builder
	sb.WriteString("version: 2\n")
	sb.WriteString("ethernets:\n")
	sb.WriteString("  metalkit0:\n")
	switch {
	case strings.HasPrefix(nc.NICSelector, "by-mac:"):
		mac := strings.TrimPrefix(nc.NICSelector, "by-mac:")
		sb.WriteString("    match:\n")
		fmt.Fprintf(&sb, "      macaddress: %q\n", strings.ToLower(mac))
	case strings.HasPrefix(nc.NICSelector, "by-name:"):
		name := strings.TrimPrefix(nc.NICSelector, "by-name:")
		sb.WriteString("    match:\n")
		fmt.Fprintf(&sb, "      name: %q\n", name)
	default:
		// auto: match any en* nic. Netplan demands at least one matcher
		// when the device name (metalkit0) is virtual.
		sb.WriteString("    match:\n")
		sb.WriteString("      name: \"en*\"\n")
	}
	if nc.VLAN > 0 {
		// Physical NIC is L2-only; IP config goes on the VLAN sub-if.
		renderVLAN(&sb, "    ", nc, b, "metalkit0")
	} else {
		writeIP(&sb, "    ", nc, b)
	}
	return sb.String()
}

func renderNetworkConfigBond(nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo) string {
	bond := nc.Bond
	var sb strings.Builder
	sb.WriteString("version: 2\n")
	// Each slave becomes its own ethernet entry. We resolve the interface
	// name to a MAC from the live NIC list for netplan match.macaddress
	// (stable across firmware renumbering). If the NIC list is unavailable,
	// fall back to match.name.
	sb.WriteString("ethernets:\n")
	slaveNames := make([]string, len(bond.Slaves))
	for i, ifname := range bond.Slaves {
		slaveNames[i] = ifname
		fmt.Fprintf(&sb, "  %s:\n", ifname)
		sb.WriteString("    match:\n")
		if mac := resolveNICMAC(nics, ifname); isValidMAC(mac) {
			fmt.Fprintf(&sb, "      macaddress: %q\n", strings.ToLower(mac))
		} else {
			fmt.Fprintf(&sb, "      name: %q\n", ifname)
		}
	}
	sb.WriteString("bonds:\n")
	sb.WriteString("  bond0:\n")
	sb.WriteString("    interfaces: [")
	for i, n := range slaveNames {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(n)
	}
	sb.WriteString("]\n")
	sb.WriteString("    parameters:\n")
	fmt.Fprintf(&sb, "      mode: %s\n", bond.Mode)
	miimon := bond.Miimon
	if miimon == 0 {
		miimon = 100
	}
	fmt.Fprintf(&sb, "      mii-monitor-interval: %d\n", miimon)
	switch bond.Mode {
	case "active-backup":
		if bond.Primary != "" {
			// Map primary interface name to ph name for netplan.
			for i, ifname := range bond.Slaves {
				if ifname == bond.Primary {
					fmt.Fprintf(&sb, "      primary: %s\n", slaveNames[i])
					break
				}
			}
		}
	case "802.3ad":
		rate := bond.LACPRate
		if rate == "" {
			rate = "fast"
		}
		fmt.Fprintf(&sb, "      lacp-rate: %s\n", rate)
		policy := bond.XmitHashPolicy
		if policy == "" {
			policy = "layer3+4"
		}
		fmt.Fprintf(&sb, "      transmit-hash-policy: %s\n", policy)
	}
	if nc.VLAN > 0 {
		// Bond is L2-only; IP config goes on the VLAN sub-if.
		renderVLAN(&sb, "    ", nc, b, "bond0")
	} else {
		writeIP(&sb, "    ", nc, b)
	}
	return sb.String()
}

// writeIP emits dhcp4 / addresses / gateway4 / nameservers under the
// given indent (matches the device key's child level). Used by both
// single-NIC and bond renderers so the IP section is identical.
func writeIP(sb *strings.Builder, indent string, nc profiles.NetworkConfig, b bindings.Binding) {
	if nc.Method == "dhcp" {
		fmt.Fprintf(sb, "%sdhcp4: true\n", indent)
		return
	}
	fmt.Fprintf(sb, "%sdhcp4: false\n", indent)
	if b.StaticAddress != "" {
		fmt.Fprintf(sb, "%saddresses: [%q]\n", indent,
			fmt.Sprintf("%s/%d", b.StaticAddress, nc.PrefixLen))
	}
	if nc.Gateway != "" {
		fmt.Fprintf(sb, "%sgateway4: %q\n", indent, nc.Gateway)
	}
	if len(nc.DNS) > 0 {
		fmt.Fprintf(sb, "%snameservers:\n", indent)
		fmt.Fprintf(sb, "%s  addresses: [", indent)
		for i, d := range nc.DNS {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(sb, "%q", d)
		}
		sb.WriteString("]\n")
	}
}

// resolveNICMAC looks up an interface name in the live NIC list and returns
// its MAC (lowercase). Returns "" if not found or nics is nil — callers
// should fall back to match.name in that case.
func resolveNICMAC(nics []NICInfo, ifname string) string {
	for _, n := range nics {
		if n.Name == ifname && n.MAC != "" {
			return strings.ToLower(n.MAC)
		}
	}
	return ""
}

// isValidMAC reports whether s looks like a MAC address (xx:xx:xx:xx:xx:xx).
// Rejects interface names and other non-MAC strings that might have leaked in.
func isValidMAC(s string) bool {
	_, err := net.ParseMAC(s)
	return err == nil
}

// renderVLAN emits a VLAN sub-interface in netplan v2 format. The parent
// device (link) is the physical NIC or bond that carries the tagged traffic.
// IP configuration goes on the VLAN device; the parent stays L2-only.
func renderVLAN(sb *strings.Builder, indent string, nc profiles.NetworkConfig, b bindings.Binding, link string) {
	fmt.Fprintf(sb, "vlans:\n")
	fmt.Fprintf(sb, "%s%s.%d:\n", indent, link, nc.VLAN)
	fmt.Fprintf(sb, "%s  id: %d\n", indent, nc.VLAN)
	fmt.Fprintf(sb, "%s  link: %s\n", indent, link)
	writeIP(sb, indent+"  ", nc, b)
}

// renderMetaData is the minimal NoCloud meta-data block.
func renderMetaData(spec jobs.InstallSpec, hostname string) string {
	jobShort := spec.JobID
	if len(jobShort) > 12 {
		jobShort = jobShort[:12]
	}
	return fmt.Sprintf("instance-id: metalkit-%s\nlocal-hostname: %q\n", jobShort, hostname)
}

// expandHostname applies the profile's HostnameTemplate substitutions:
//
//   - {serial}  → first 8 chars of binding.MachineUUID (we don't fetch the
//                 real SMBIOS serial in M2.3; documented in plan §B5).
//   - {uuid8}   → first 8 chars of binding.MachineUUID.
//   - {mac}     → left literal — agent doesn't reliably know its MAC at
//                 install time without a NIC selector match; documented.
//
// If binding.Hostname is non-empty it overrides the template entirely.
func expandHostname(template string, b bindings.Binding, _ profiles.Profile) string {
	if strings.TrimSpace(b.Hostname) != "" {
		return strings.TrimSpace(b.Hostname)
	}
	short := b.MachineUUID
	if len(short) > 8 {
		short = short[:8]
	}
	out := template
	out = strings.ReplaceAll(out, "{serial}", short)
	out = strings.ReplaceAll(out, "{uuid8}", short)
	// {mac} intentionally not substituted; see doc above.
	return out
}
