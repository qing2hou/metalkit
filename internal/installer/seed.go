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
	"metalkit/internal/components"
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

	userPath := filepath.Join(seedDir, "user-data")
	metaPath := filepath.Join(seedDir, "meta-data")
	if err := deps.FS.WriteFile(userPath, []byte(userData), 0o644); err != nil {
		return fmt.Errorf("install: write user-data: %w", err)
	}
	if err := deps.FS.WriteFile(metaPath, []byte(metaData), 0o644); err != nil {
		return fmt.Errorf("install: write meta-data: %w", err)
	}

	// Remove pre-existing network config files that would conflict with
	// our config. CentOS 7 images ship with ifcfg-eth0 (DHCP). This MUST
	// run before writeIfcfg so we don't delete our own ifcfg-eth0.
	cleanStaleNetworkConfig(ctx, deps, mntRoot)

	// Resolve the effective network renderer: explicit selection > OS-family
	// auto-detection. Then dispatch to the appropriate rendering path.
	renderer := resolveRenderer(spec.NetworkRenderer, deps, mntRoot)

	// Cloud-init's NetworkManager renderer has a known bug with bond
	// configurations on RockyLinux 8+ / RHEL 8+: it generates duplicate NM
	// connection files — one bond-slave (using the YAML key name) and one
	// plain ethernet (using the MAC-matched interface name). NM picks the
	// plain one, so slaves never join the bond. Bypass cloud-init's network
	// rendering entirely for NM + bond and write NM keyfiles directly.
	bypassCloudInitNetwork := renderer == components.NetworkManagerRenderer && spec.Profile.Network.Bond != nil

	rhel7 := isRHEL7Root(deps, mntRoot)

	if !bypassCloudInitNetwork {
		// Write the network-config v2 so cloud-init has something to
		// process. Individual renderers may also write direct config files as
		// fallbacks or override cloud-init's renderer preference.
		netCfg := renderNetworkConfig(spec.Profile.Network, spec.Binding, deps.NICs, rhel7)
		netPath := filepath.Join(seedDir, "network-config")
		if err := deps.FS.WriteFile(netPath, []byte(netCfg), 0o644); err != nil {
			return fmt.Errorf("install: write network-config: %w", err)
		}
	}

	// Renderer-specific post-processing.
	switch renderer {
	case components.NetPlanRenderer:
		// Netplan is the default cloud-init path — no extra work needed.
	case components.NetworkManagerRenderer:
		if bypassCloudInitNetwork {
			writeNMKeyfileBond(deps, mntRoot, spec.Profile.Network, spec.Binding, deps.NICs)
			disableCloudInitNetwork(deps, mntRoot)
		} else {
			writeNMRendererConfig(deps, mntRoot)
		}
	case components.SysconfigRenderer:
		if spec.Profile.Network.Bond == nil {
			writeIfcfg(deps, mntRoot, spec.Profile.Network, spec.Binding, deps.NICs)
		} else {
			writeIfcfgBond(deps, mntRoot, spec.Profile.Network, spec.Binding, deps.NICs)
		}
	case components.ENIRenderer:
		writeENIRendererConfig(deps, spec, mntRoot)
	case components.WickedRenderer:
		writeWickedConfig(deps, spec, mntRoot)
	}

	// Some cloud images (CentOS 7.9, RHEL) ship with cloud-init but
	// without the NoCloud datasource in their datasource_list.  Without
	// this file cloud-init ignores /var/lib/cloud/seed/nocloud-net/ and
	// the machine boots with the image defaults (root locked, no SSH
	// password auth).  We write the override directly into the rootfs so
	// it takes effect on first boot — chicken-and-egg problem with
	// write_files in user-data which runs AFTER datasource init.
	dsDir := filepath.Join(mntRoot, "etc", "cloud", "cloud.cfg.d")
	if err := deps.FS.MkdirAll(dsDir, 0o755); err != nil {
		return fmt.Errorf("install: mkdir cloud.cfg.d: %w", err)
	}
	dsPath := filepath.Join(dsDir, "99-metalkit.cfg")
	dsContent := "# Managed by metalkit installer — enables NoCloud seed discovery.\ndatasource_list: [ NoCloud, None ]\n"
	if err := deps.FS.WriteFile(dsPath, []byte(dsContent), 0o644); err != nil {
		return fmt.Errorf("install: write datasource config: %w", err)
	}

	// RHEL family (CentOS / RHEL / Rocky / AlmaLinux / Fedora / openEuler)
	// ships with SELinux in enforcing mode. Files we wrote into the rootfs
	// from the live system inherit the mount-time context, which is wrong.
	//
	// Previously we touched /.autorelabel to trigger a full SELinux relabel
	// on first boot, but on large disks (500G+) this takes a very long time
	// with no visible progress under Plymouth, making it look like the
	// system is hung.
	//
	// Instead, run restorecon on only the directories we actually modified.
	// This is fast (seconds, not hours) because it skips the bulk of the
	// filesystem that already has correct contexts from the cloud image.
	// The /.autorelabel fallback remains for cases where restorecon fails.
	//
	// Skip entirely if we plan to disable SELinux in installGRUBChrootRHEL
	// (see grub.go disableSELinuxIfEnforcing). When SELinux is disabled
	// at boot, contexts don't matter, and the /.autorelabel fallback
	// here would trigger a full-disk relabel on first boot — which on
	// large XFS partitions takes 30+ minutes with no Plymouth progress
	// display, looking like a hang (qa.md #4).
	if isRHELFamilyRoot(deps, mntRoot) && !selinuxWillBeDisabled(deps, mntRoot) {
		// Paths we wrote into the rootfs that need SELinux relabeling.
		relabelPaths := []string{
			"/etc/cloud",
			"/etc/NetworkManager",
			"/etc/sysconfig/network-scripts",
			"/etc/network",
			"/etc/ssh",
			"/etc/hostname",
			"/etc/shadow",
			"/etc/fstab",
			"/etc/sysconfig/network",
			"/var/lib/cloud",
		}
		args := append([]string{mntRoot, "restorecon", "-RF"}, relabelPaths...)
		if out, err := deps.Exec.Run(ctx, "chroot", args...); err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("restorecon failed, falling back to /.autorelabel", "err", err, "output", string(out))
			}
			// Fallback: touch /.autorelabel so SELinux relabels on next boot.
			relabelPath := filepath.Join(mntRoot, ".autorelabel")
			_ = deps.FS.WriteFile(relabelPath, nil, 0o644)
		} else if deps.Logger != nil {
			deps.Logger.Info("restorecon: relabeled metalkit-modified paths")
		}
	}

	// When cloud-init is absent from the target rootfs, the NoCloud seed
	// files we wrote above will never be consumed. In that case we must
	// write the configuration directly into the rootfs so the machine
	// boots with the correct hostname, root password, network config and
	// SSH settings. When cloud-init IS present, these direct writes are
	// still safe — cloud-init will overwrite them on first boot if its
	// seed data differs, and identical content causes no harm.
	if !hasCloudInit(deps, mntRoot) {
		if deps.Logger != nil {
			deps.Logger.Info("cloud-init not found in target rootfs, writing config directly")
		}
		writeDirectHostname(deps, mntRoot, host)
		writeDirectPassword(deps, mntRoot, spec.Profile.RootPasswordHash)
		writeDirectSSHDConfig(deps, mntRoot)
		writeDirectNetworkConfig(deps, spec, mntRoot)
	}

	return nil
}

// resolveRenderer determines the effective network renderer. If the profile
// has an explicit selection (non-empty), it wins. Otherwise we fall back to
// OS-detection heuristics that replicate the pre-component-selection behavior.
func resolveRenderer(explicit string, deps Deps, mntRoot string) components.NetworkRenderer {
	if explicit != "" {
		return components.NetworkRenderer(explicit)
	}
	// Auto-detect: replicate the old hard-coded logic as defaults.
	if isRHEL7Root(deps, mntRoot) {
		return components.SysconfigRenderer
	}
	if prefersNetworkManagerRenderer(deps, mntRoot) {
		return components.NetworkManagerRenderer
	}
	return components.NetPlanRenderer
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
// PasswordAuthentication to `yes`. A runcmd then restarts sshd (or
// ssh — service name varies by distro) so the change takes effect on
// first boot.
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
	// usermod -p is the only reliable way to set the root password on
	// CentOS 7 — its cloud-init 19.4 ignores the chpasswd block above.
	fmt.Fprintf(&b, "  - [usermod, -p, %q, root]\n", spec.Profile.RootPasswordHash)
	// Restart sshd if present (Debian/Ubuntu/RHEL all use this unit name
	// in recent releases). The `|| true` shell wrapper keeps the runcmd
	// script's exit code at 0 — otherwise a missing unit (e.g. Ubuntu's
	// `ssh.service` vs CentOS 7's `sshd.service`) fails the whole script
	// and cloud-init logs it as a scripts-user error.
	b.WriteString("  - [sh, -c, \"systemctl restart sshd 2>/dev/null || systemctl restart ssh 2>/dev/null || true\"]\n")
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
func renderNetworkConfig(nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo, rhel7 bool) string {
	if nc.Bond != nil {
		return renderNetworkConfigBond(nc, b, nics, rhel7)
	}
	return renderNetworkConfigSingle(nc, b, nics, rhel7)
}

// resolveSingleNICKey picks the YAML key for the single-NIC netplan stanza.
// When the operator selected a specific NIC (by-mac / by-name) and the agent
// reported the live NIC list, we use the kernel-visible name (eno2, ens3, ...)
// so the rendered file reads naturally — "ethernets: eno2:" instead of the
// generic "ethernets: eth0:". match.macaddress still pins the binding
// physically; the YAML key is purely cosmetic for netplan.
//
// Caveat for RHEL 7: CentOS 7 / RHEL 7 cloud images boot the installed OS
// with `net.ifnames=0 biosdevname=0` on the kernel cmdline, so kernel rewrites
// every NIC to ethN regardless of what the live agent saw (eno2/ens1np0/...).
// On those rootfs we cannot pass the live name into the netplan YAML key:
// cloud-init's sysconfig renderer pipes it straight into DEVICE=, and the
// installed kernel will never produce a device with that name. The caller
// signals this with rhel7=true and we fall back to "eth<index>" using the
// picked NIC's position in the live list (which mirrors the kernel's
// ifindex ordering — same order CentOS 7 will see at first boot).
//
// Auto mode (or no live match) falls back to "eth0" — netplan treats it as a
// logical alias of any en* device via the wildcard matcher.
func resolveSingleNICKey(selector string, nics []NICInfo, rhel7 bool) string {
	switch {
	case strings.HasPrefix(selector, "by-mac:"):
		mac := strings.ToLower(strings.TrimPrefix(selector, "by-mac:"))
		for i, n := range nics {
			if strings.ToLower(n.MAC) == mac && n.Name != "" {
				if rhel7 {
					return fmt.Sprintf("eth%d", i)
				}
				return n.Name
			}
		}
	case strings.HasPrefix(selector, "by-name:"):
		name := strings.TrimPrefix(selector, "by-name:")
		// by-name selectors may be a real kernel name (eno2) or a MAC
		// (validateIFName accepts both). If it's a MAC, resolve to the
		// live kernel name; otherwise pass through.
		if isValidMAC(name) {
			for i, n := range nics {
				if strings.EqualFold(n.MAC, name) && n.Name != "" {
					if rhel7 {
						return fmt.Sprintf("eth%d", i)
					}
					return n.Name
				}
			}
		} else if name != "" {
			return name
		}
	}
	return "eth0"
}

func renderNetworkConfigSingle(nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo, rhel7 bool) string {
	var sb strings.Builder
	sb.WriteString("version: 2\n")
	sb.WriteString("ethernets:\n")
	key := resolveSingleNICKey(nc.NICSelector, nics, rhel7)
	fmt.Fprintf(&sb, "  %s:\n", key)
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
		renderVLAN(&sb, "    ", nc, b, key)
	} else {
		writeIP(&sb, "    ", nc, b)
	}
	return sb.String()
}

func renderNetworkConfigBond(nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo, rhel7 bool) string {
	bond := nc.Bond
	var sb strings.Builder
	sb.WriteString("version: 2\n")

	// Resolve each slave to its real kernel-visible NIC name for the YAML key.
	// cloud-init's NetworkManager renderer uses the YAML key as the NM
	// connection ID and interface-name, so it must match the real device name
	// in the installed OS. MAC matching (match.macaddress) still pins the
	// config to the correct physical NIC.
	//
	// For RHEL7 (net.ifnames=0), the installed OS rewrites all NICs to ethN,
	// so we use eth<index> based on the NIC's position in the live list —
	// matching the kernel's ifindex ordering that CentOS 7 will follow.
	sb.WriteString("ethernets:\n")
	slaveKeys := make([]string, len(bond.Slaves))
	for i, slave := range bond.Slaves {
		mac := lookupSlaveMAC(slave, nics)
		var key string
		if isValidMAC(mac) {
			if rhel7 {
				// Find the NIC index for RHEL7 ethN naming.
				for j, n := range nics {
					if strings.EqualFold(n.MAC, mac) {
						key = fmt.Sprintf("eth%d", j)
						break
					}
				}
			} else {
				// Use the live kernel name (eth0, eno1, ens3, ...).
				for _, n := range nics {
					if strings.EqualFold(n.MAC, mac) && n.Name != "" {
						key = n.Name
						break
					}
				}
			}
		} else {
			// Slave is an interface name, not a MAC.
			key = slave
		}
		if key == "" {
			key = fmt.Sprintf("eth%d", i)
		}
		slaveKeys[i] = key
		fmt.Fprintf(&sb, "  %s:\n", key)
		sb.WriteString("    match:\n")
		if isValidMAC(mac) {
			fmt.Fprintf(&sb, "      macaddress: %q\n", strings.ToLower(mac))
		} else {
			fmt.Fprintf(&sb, "      name: %q\n", slave)
		}
	}
	sb.WriteString("bonds:\n")
	sb.WriteString("  bond0:\n")
	sb.WriteString("    interfaces: [")
	for i, n := range slaveKeys {
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
			for i, s := range bond.Slaves {
				if s == bond.Primary {
					fmt.Fprintf(&sb, "      primary: %s\n", slaveKeys[i])
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

// lookupSlaveMAC returns the MAC for a bond slave specifier. If the slave is
// already a MAC address (xx:xx:xx:xx:xx:xx) it is returned directly. Otherwise
// it is treated as an interface name and resolved through the live NIC list.
// Returns "" when the name cannot be resolved — callers should fall back to
// match.name.
func lookupSlaveMAC(slave string, nics []NICInfo) string {
	if isValidMAC(slave) {
		return strings.ToLower(slave)
	}
	return resolveNICMAC(nics, slave)
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

// cleanStaleNetworkConfig removes image-baked network configuration files that
// would conflict with the cloud-init-rendered network config. For example,
// CentOS 7 cloud images ship ifcfg-eth0 (DHCP) — if left in place it races
// with the ifcfg-metalkit0 written by cloud-init during NoCloud datasource
// processing, and NetworkManager may pick the DHCP file instead of our static
// config.
func cleanStaleNetworkConfig(_ context.Context, deps Deps, mntRoot string) {
	// CentOS / RHEL sysconfig ifcfg files.
	patterns := []string{
		filepath.Join(mntRoot, "etc", "sysconfig", "network-scripts", "ifcfg-eth*"),
		filepath.Join(mntRoot, "etc", "sysconfig", "network-scripts", "ifcfg-eno*"),
		filepath.Join(mntRoot, "etc", "sysconfig", "network-scripts", "ifcfg-ens*"),
	}
	for _, pat := range patterns {
		matches, err := filepath.Glob(pat)
		if err != nil {
			continue
		}
		for _, p := range matches {
			_ = deps.FS.Remove(p)
		}
	}

	// NetworkManager connection keyfiles. Cloud images (Rocky 8+, RHEL 8+,
	// Fedora) ship with pre-configured NM connections that race with our
	// cloud-init-rendered config. For example, a default DHCP connection on
	// eth0 would prevent NM from enslaving eth0 to bond0.
	nmDir := filepath.Join(mntRoot, "etc", "NetworkManager", "system-connections")
	if deps.FS.Exists(nmDir) {
		matches, err := filepath.Glob(filepath.Join(nmDir, "*.nmconnection"))
		if err == nil {
			for _, p := range matches {
				_ = deps.FS.Remove(p)
			}
		}
	}
}

// isRHELFamilyRoot detects any RHEL-family distribution (CentOS, RHEL,
// Rocky, AlmaLinux, Fedora, openEuler) from /etc/os-release inside the mounted
// rootfs. Used to decide whether SELinux autorelabel is needed on first boot —
// all RHEL-family distros ship with SELinux in enforcing mode.
func isRHELFamilyRoot(deps Deps, mntRoot string) bool {
	data, err := deps.FS.ReadFile(filepath.Join(mntRoot, "etc", "os-release"))
	if err != nil {
		return false
	}
	c := string(data)
	cLower := strings.ToLower(c)
	for _, marker := range []string{
		`id="centos"`,
		`id="rhel"`,
		`id="rocky"`,
		`id="almalinux"`,
		`id="fedora"`,
		`id="openeuler"`,
		`id_like="rhel`,
		`id_like="fedora`,
		`id_like=rhel`,
		`id_like=fedora`,
	} {
		if strings.Contains(cLower, marker) {
			return true
		}
	}
	return false
}

// selinuxWillBeDisabled returns true if /etc/selinux/config in the target
// rootfs has SELINUX=disabled (or no SELinux config at all). Used to skip
// restorecon/autorelabel logic in BuildSeed — see comment there. Note this
// reports the *current* state of the config file, which is what we want:
// installGRUBChrootRHEL disables SELinux *after* BuildSeed runs (so during
// BuildSeed SELinux may still be "enforcing" in config), but a MetalKit-
// prepared image (see IMAGE-PREP.md) already has SELinux=disabled before
// install, and in that case we want to skip relabel here.
func selinuxWillBeDisabled(deps Deps, mntRoot string) bool {
	data, err := deps.FS.ReadFile(filepath.Join(mntRoot, "etc", "selinux", "config"))
	if err != nil {
		return true // no selinux config → effectively disabled
	}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "SELINUX=") {
			val := strings.TrimSpace(strings.TrimPrefix(t, "SELINUX="))
			return val == "disabled"
		}
	}
	return false
}

// disableCloudInitNetwork writes a cloud.cfg.d drop-in that tells cloud-init
// to skip network rendering entirely. Used when we write NM keyfiles directly
// (e.g. for bond configs where cloud-init's NM renderer is buggy).
func disableCloudInitNetwork(deps Deps, mntRoot string) {
	dsDir := filepath.Join(mntRoot, "etc", "cloud", "cloud.cfg.d")
	if !deps.FS.Exists(dsDir) {
		_ = deps.FS.MkdirAll(dsDir, 0o755)
	}
	path := filepath.Join(dsDir, "97-metalkit-no-network.cfg")
	content := "# Managed by metalkit installer — NM keyfiles written directly.\n# Disable cloud-init network rendering to avoid conflicts.\nnetwork: {config: disabled}\n"
	_ = deps.FS.WriteFile(path, []byte(content), 0o644)
}

// prefersNetworkManagerRenderer reports whether the target rootfs should
// have cloud-init emit NetworkManager keyfiles instead of legacy ifcfg.
// True for modern RHEL family (Rocky / AlmaLinux / RHEL 8+ / Fedora / openEuler);
// false for CentOS 7 / RHEL 7 (no NM renderer in their cloud-init) and
// for non-RHEL distros (Ubuntu uses netplan natively).
func prefersNetworkManagerRenderer(deps Deps, mntRoot string) bool {
	if isOpenEulerRoot(deps, mntRoot) {
		return true
	}
	if !isRHELFamilyRoot(deps, mntRoot) {
		return false
	}
	if isRHEL7Root(deps, mntRoot) {
		return false
	}
	return true
}

// isOpenEulerRoot detects openEuler from /etc/os-release inside the mounted rootfs.
func isOpenEulerRoot(deps Deps, mntRoot string) bool {
	data, err := deps.FS.ReadFile(filepath.Join(mntRoot, "etc", "os-release"))
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(data)), `id="openeuler"`)
}

// isKylinRoot detects 银河麒麟 (Kylin) from /etc/os-release inside the mounted rootfs.
func isKylinRoot(deps Deps, mntRoot string) bool {
	data, err := deps.FS.ReadFile(filepath.Join(mntRoot, "etc", "os-release"))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), `ID="kylin"`)
}

// isOpenSUSERoot detects openSUSE from /etc/os-release inside the mounted rootfs.
func isOpenSUSERoot(deps Deps, mntRoot string) bool {
	data, err := deps.FS.ReadFile(filepath.Join(mntRoot, "etc", "os-release"))
	if err != nil {
		return false
	}
	c := string(data)
	return strings.Contains(c, `ID="opensuse-leap"`) ||
		strings.Contains(c, `ID="opensuse-tumbleweed"`) ||
		strings.Contains(c, `ID="opensuse"`)
}

// writeNMRendererConfig pins cloud-init's network renderer to NetworkManager
// by writing /etc/cloud/cloud.cfg.d/98-metalkit-renderer.cfg.
func writeNMRendererConfig(deps Deps, mntRoot string) {
	dsDir := filepath.Join(mntRoot, "etc", "cloud", "cloud.cfg.d")
	if !deps.FS.Exists(dsDir) {
		_ = deps.FS.MkdirAll(dsDir, 0o755)
	}
	rendererPath := filepath.Join(dsDir, "98-metalkit-renderer.cfg")
	rendererContent := "# Managed by metalkit installer — prefer NetworkManager keyfiles over legacy ifcfg.\n" +
		"system_info:\n" +
		"  network:\n" +
		"    renderers: [ network-manager, sysconfig ]\n"
	_ = deps.FS.WriteFile(rendererPath, []byte(rendererContent), 0o644)
}

// writeENIRendererConfig configures cloud-init to prefer the ENI renderer
// (writes /etc/network/interfaces) and also writes the interfaces file
// directly as a fallback for systems without cloud-init.
func writeENIRendererConfig(deps Deps, spec jobs.InstallSpec, mntRoot string) {
	nc := spec.Profile.Network
	b := spec.Binding
	nics := deps.NICs

	// Pin cloud-init renderer to ENI.
	dsDir := filepath.Join(mntRoot, "etc", "cloud", "cloud.cfg.d")
	if !deps.FS.Exists(dsDir) {
		_ = deps.FS.MkdirAll(dsDir, 0o755)
	}
	rendererPath := filepath.Join(dsDir, "98-metalkit-renderer.cfg")
	rendererContent := "# Managed by metalkit installer — prefer ENI /etc/network/interfaces.\n" +
		"system_info:\n" +
		"  network:\n" +
		"    renderers: [ eni, netplan ]\n"
	_ = deps.FS.WriteFile(rendererPath, []byte(rendererContent), 0o644)

	// Write /etc/network/interfaces directly as fallback.
	if nc.Bond != nil {
		writeENIInterfacesBond(deps, mntRoot, nc, b, nics)
	} else {
		writeENIInterfaces(deps, mntRoot, nc, b, nics)
	}
}

// writeENIInterfaces writes a single-NIC /etc/network/interfaces file.
func writeENIInterfaces(deps Deps, mntRoot string, nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo) {
	ifDir := filepath.Join(mntRoot, "etc", "network")
	if !deps.FS.Exists(ifDir) {
		_ = deps.FS.MkdirAll(ifDir, 0o755)
	}

	// Resolve the interface name.
	dev := "eth0"
	switch {
	case strings.HasPrefix(nc.NICSelector, "by-mac:"):
		mac := strings.ToLower(strings.TrimPrefix(nc.NICSelector, "by-mac:"))
		for _, n := range nics {
			if strings.ToLower(n.MAC) == mac && n.Name != "" {
				dev = n.Name
				break
			}
		}
	case strings.HasPrefix(nc.NICSelector, "by-name:"):
		name := strings.TrimPrefix(nc.NICSelector, "by-name:")
		if isValidMAC(name) {
			for _, n := range nics {
				if strings.EqualFold(n.MAC, name) && n.Name != "" {
					dev = n.Name
					break
				}
			}
		} else if name != "" {
			dev = name
		}
	}

	var lines []string
	lines = append(lines, "# Managed by metalkit installer")
	lines = append(lines, "source /etc/network/interfaces.d/*")
	lines = append(lines, "")
	lines = append(lines, "auto lo")
	lines = append(lines, "iface lo inet loopback")
	lines = append(lines, "")
	lines = append(lines, "auto "+dev)

	if nc.Method == "dhcp" {
		lines = append(lines, "iface "+dev+" inet dhcp")
	} else {
		lines = append(lines, "iface "+dev+" inet static")
		if b.StaticAddress != "" {
			lines = append(lines, "    address "+b.StaticAddress)
			if nc.PrefixLen > 0 {
				lines = append(lines, fmt.Sprintf("    netmask %s", prefixToNetmask(nc.PrefixLen)))
			}
		}
		if nc.Gateway != "" {
			lines = append(lines, "    gateway "+nc.Gateway)
		}
		if len(nc.DNS) > 0 {
			lines = append(lines, "    dns-nameservers "+strings.Join(nc.DNS, " "))
		}
	}

	content := strings.Join(lines, "\n") + "\n"
	_ = deps.FS.WriteFile(filepath.Join(ifDir, "interfaces"), []byte(content), 0o644)
}

// writeENIInterfacesBond writes /etc/network/interfaces with bond configuration.
func writeENIInterfacesBond(deps Deps, mntRoot string, nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo) {
	ifDir := filepath.Join(mntRoot, "etc", "network")
	if !deps.FS.Exists(ifDir) {
		_ = deps.FS.MkdirAll(ifDir, 0o755)
	}

	bond := nc.Bond
	miimon := bond.Miimon
	if miimon == 0 {
		miimon = 100
	}

	var lines []string
	lines = append(lines, "# Managed by metalkit installer")
	lines = append(lines, "source /etc/network/interfaces.d/*")
	lines = append(lines, "")
	lines = append(lines, "auto lo")
	lines = append(lines, "iface lo inet loopback")
	lines = append(lines, "")

	// Slave interfaces.
	for i, slave := range bond.Slaves {
		dev := slave
		if isValidMAC(slave) {
			for _, n := range nics {
				if strings.EqualFold(n.MAC, slave) && n.Name != "" {
					dev = n.Name
					break
				}
			}
		}
		lines = append(lines, "auto "+dev)
		lines = append(lines, "iface "+dev+" inet manual")
		if i == 0 {
			lines = append(lines, fmt.Sprintf("    bond-master bond0"))
		} else {
			lines = append(lines, fmt.Sprintf("    bond-master bond0"))
		}
		lines = append(lines, "")
	}

	// Bond master.
	lines = append(lines, "auto bond0")
	if nc.Method == "dhcp" {
		lines = append(lines, "iface bond0 inet dhcp")
	} else {
		lines = append(lines, "iface bond0 inet static")
		if b.StaticAddress != "" {
			lines = append(lines, "    address "+b.StaticAddress)
			if nc.PrefixLen > 0 {
				lines = append(lines, fmt.Sprintf("    netmask %s", prefixToNetmask(nc.PrefixLen)))
			}
		}
		if nc.Gateway != "" {
			lines = append(lines, "    gateway "+nc.Gateway)
		}
		if len(nc.DNS) > 0 {
			lines = append(lines, "    dns-nameservers "+strings.Join(nc.DNS, " "))
		}
	}
	lines = append(lines, fmt.Sprintf("    bond-mode %s", bond.Mode))
	lines = append(lines, fmt.Sprintf("    bond-miimon %d", miimon))
	switch bond.Mode {
	case "active-backup":
		if bond.Primary != "" {
			lines = append(lines, "    bond-primary "+bond.Primary)
		}
	case "802.3ad":
		rate := bond.LACPRate
		if rate == "" {
			rate = "fast"
		}
		lines = append(lines, "    bond-lacp-rate "+rate)
		policy := bond.XmitHashPolicy
		if policy == "" {
			policy = "layer3+4"
		}
		lines = append(lines, "    bond-xmit-hash-policy "+policy)
	}

	content := strings.Join(lines, "\n") + "\n"
	_ = deps.FS.WriteFile(filepath.Join(ifDir, "interfaces"), []byte(content), 0o644)
}

// writeWickedConfig writes openSUSE Wicked network configuration files
// directly into /etc/sysconfig/network/ifcfg-* and configures cloud-init
// to use the sysconfig renderer (which Wicked consumes on openSUSE).
func writeWickedConfig(deps Deps, spec jobs.InstallSpec, mntRoot string) {
	nc := spec.Profile.Network
	b := spec.Binding
	nics := deps.NICs

	// Pin cloud-init to sysconfig renderer (Wicked consumes ifcfg files).
	dsDir := filepath.Join(mntRoot, "etc", "cloud", "cloud.cfg.d")
	if !deps.FS.Exists(dsDir) {
		_ = deps.FS.MkdirAll(dsDir, 0o755)
	}
	rendererPath := filepath.Join(dsDir, "98-metalkit-renderer.cfg")
	rendererContent := "# Managed by metalkit installer — prefer sysconfig renderer for Wicked.\n" +
		"system_info:\n" +
		"  network:\n" +
		"    renderers: [ sysconfig, netplan ]\n"
	_ = deps.FS.WriteFile(rendererPath, []byte(rendererContent), 0o644)

	// Write Wicked-style ifcfg files directly as fallback.
	sysDir := filepath.Join(mntRoot, "etc", "sysconfig", "network")
	if !deps.FS.Exists(sysDir) {
		_ = deps.FS.MkdirAll(sysDir, 0o755)
	}

	if nc.Bond != nil {
		writeWickedBond(deps, mntRoot, nc, b, nics, sysDir)
	} else {
		writeWickedIfcfg(deps, mntRoot, nc, b, nics, sysDir)
	}
}

// writeWickedIfcfg writes a single-NIC openSUSE Wicked ifcfg file.
func writeWickedIfcfg(deps Deps, mntRoot string, nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo, sysDir string) {
	// Resolve the interface name.
	dev := "eth0"
	var hwaddr string
	switch {
	case strings.HasPrefix(nc.NICSelector, "by-mac:"):
		mac := strings.ToLower(strings.TrimPrefix(nc.NICSelector, "by-mac:"))
		hwaddr = mac
		for _, n := range nics {
			if strings.ToLower(n.MAC) == mac && n.Name != "" {
				dev = n.Name
				break
			}
		}
	case strings.HasPrefix(nc.NICSelector, "by-name:"):
		name := strings.TrimPrefix(nc.NICSelector, "by-name:")
		if isValidMAC(name) {
			hwaddr = strings.ToLower(name)
			for _, n := range nics {
				if strings.EqualFold(n.MAC, name) && n.Name != "" {
					dev = n.Name
					break
				}
			}
		} else if name != "" {
			dev = name
		}
	}

	var lines []string
	lines = append(lines, "# Managed by metalkit installer")
	lines = append(lines, "BOOTPROTO='"+ternary(nc.Method == "dhcp", "dhcp", "static")+"'")
	if nc.Method != "dhcp" {
		if b.StaticAddress != "" {
			lines = append(lines, "IPADDR='"+b.StaticAddress+"'")
		}
		if nc.PrefixLen > 0 {
			lines = append(lines, fmt.Sprintf("PREFIXLEN='%d'", nc.PrefixLen))
		}
		if nc.Gateway != "" {
			lines = append(lines, "GATEWAY='"+nc.Gateway+"'")
		}
		if len(nc.DNS) > 0 {
			lines = append(lines, fmt.Sprintf("DNS='%s'", strings.Join(nc.DNS, " ")))
		}
	}
	if hwaddr != "" {
		lines = append(lines, "LLADDR='"+strings.ToUpper(hwaddr)+"'")
	}
	lines = append(lines, "STARTMODE='auto'")

	content := strings.Join(lines, "\n") + "\n"
	_ = deps.FS.WriteFile(filepath.Join(sysDir, "ifcfg-"+dev), []byte(content), 0o644)
}

// writeWickedBond writes Wicked ifcfg files for a bond master and its slaves.
func writeWickedBond(deps Deps, mntRoot string, nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo, sysDir string) {
	bond := nc.Bond
	miimon := bond.Miimon
	if miimon == 0 {
		miimon = 100
	}

	// Slave ifcfg files.
	for i, slave := range bond.Slaves {
		dev := slave
		var hwaddr string
		if isValidMAC(slave) {
			hwaddr = strings.ToLower(slave)
			for _, n := range nics {
				if strings.EqualFold(n.MAC, slave) && n.Name != "" {
					dev = n.Name
					break
				}
			}
		}
		var lines []string
		lines = append(lines, "# Managed by metalkit installer")
		lines = append(lines, "BOOTPROTO='none'")
		if hwaddr != "" {
			lines = append(lines, "LLADDR='"+strings.ToUpper(hwaddr)+"'")
		}
		lines = append(lines, "STARTMODE='auto'")
		lines = append(lines, fmt.Sprintf("BONDING_MASTER='%d'", 1))
		if i == 0 {
			lines = append(lines, fmt.Sprintf("BONDING_SLAVE_0='%s'", dev))
		}
		_ = deps.FS.WriteFile(
			filepath.Join(sysDir, "ifcfg-"+dev),
			[]byte(strings.Join(lines, "\n")+"\n"),
			0o644,
		)
	}

	// Bond master ifcfg-bond0.
	var master []string
	master = append(master, "# Managed by metalkit installer")
	master = append(master, "BOOTPROTO='"+ternary(nc.Method == "dhcp", "dhcp", "static")+"'")
	if nc.Method != "dhcp" {
		if b.StaticAddress != "" {
			master = append(master, "IPADDR='"+b.StaticAddress+"'")
		}
		if nc.PrefixLen > 0 {
			master = append(master, fmt.Sprintf("PREFIXLEN='%d'", nc.PrefixLen))
		}
		if nc.Gateway != "" {
			master = append(master, "GATEWAY='"+nc.Gateway+"'")
		}
	}
	master = append(master, "STARTMODE='auto'")
	master = append(master, fmt.Sprintf("BONDING_MODULE_OPTS='mode=%s miimon=%d'", bond.Mode, miimon))
	switch bond.Mode {
	case "active-backup":
		if bond.Primary != "" {
			master = append(master, fmt.Sprintf("BONDING_PRIMARY='%s'", bond.Primary))
		}
	case "802.3ad":
		rate := bond.LACPRate
		if rate == "" {
			rate = "fast"
		}
		policy := bond.XmitHashPolicy
		if policy == "" {
			policy = "layer3+4"
		}
		master = append(master, fmt.Sprintf("BONDING_MODULE_OPTS='mode=%s miimon=%d lacp_rate=%s xmit_hash_policy=%s'",
			bond.Mode, miimon, rate, policy))
	}
	for i, slave := range bond.Slaves {
		master = append(master, fmt.Sprintf("BONDING_SLAVE_%d='%s'", i, slave))
	}

	_ = deps.FS.WriteFile(
		filepath.Join(sysDir, "ifcfg-bond0"),
		[]byte(strings.Join(master, "\n")+"\n"),
		0o644,
	)
}

// prefixToNetmask converts a CIDR prefix length (1-32) to a dotted-decimal
// netmask string (e.g. 24 → "255.255.255.0").
func prefixToNetmask(prefix int) string {
	if prefix < 0 || prefix > 32 {
		return "255.255.255.0"
	}
	mask := net.CIDRMask(prefix, 32)
	return mask.String()
}

// ternary returns a if cond is true, otherwise b.
func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// isRHEL7Root detects CentOS / RHEL 7 from /etc/os-release inside the mounted
// rootfs. Cloud-init 19.4 on those distributions does not fully support
// network-config v2; we write the ifcfg file directly instead.
func isRHEL7Root(deps Deps, mntRoot string) bool {
	data, err := deps.FS.ReadFile(filepath.Join(mntRoot, "etc", "os-release"))
	if err != nil {
		return false
	}
	content := string(data)
	hasRHEL := strings.Contains(content, `ID="centos"`) ||
		strings.Contains(content, `ID="rhel"`) ||
		strings.Contains(content, `ID="rocky"`) ||
		strings.Contains(content, "ID_LIKE=\"rhel")
	hasV7 := strings.Contains(content, `VERSION_ID="7`) // matches "7" and "7.9"
	return hasRHEL && hasV7
}

// writeIfcfg writes a RHEL 7-style ifcfg file directly into the mounted
// rootfs at /etc/sysconfig/network-scripts/ifcfg-eth<N>. This bypasses
// cloud-init's network-config v2 renderer which CentOS 7's cloud-init 19.4
// cannot process. HWADDR matching ensures the config binds to the correct
// NIC regardless of the kernel's device naming.
//
// NIC selection rules:
//   - by-mac:<MAC>: write ifcfg for that exact NIC; the file is named
//     ifcfg-eth<N> where N is the picked MAC's position in the live NIC
//     list. CentOS 7 boots with net.ifnames=0, so the kernel discovery
//     order (which the agent reflects) matches the eth<N> ordering the
//     installed OS will produce. HWADDR pins the IP to the right NIC even
//     if the index ends up off by one.
//   - by-name:<MAC>: same handling as by-mac when name is a MAC.
//   - everything else (auto / by-name with a real ifname): fall back to
//     ifcfg-eth0 with no HWADDR — agent picks the first up NIC at the
//     installed OS layer too, so the alphabetical first ethN wins.
func writeIfcfg(deps Deps, mntRoot string, nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo) {
	sysDir := filepath.Join(mntRoot, "etc", "sysconfig", "network-scripts")
	if !deps.FS.Exists(sysDir) {
		_ = deps.FS.MkdirAll(sysDir, 0o755)
	}

	// Resolve the target MAC + post-install eth index from the live NIC list.
	// When the selector doesn't pick a specific NIC (auto), we fall back to
	// eth0 with no HWADDR so initscripts simply use the first NIC.
	mac := ""
	ethIdx := 0
	switch {
	case strings.HasPrefix(nc.NICSelector, "by-mac:"):
		mac = strings.ToLower(strings.TrimPrefix(nc.NICSelector, "by-mac:"))
	case strings.HasPrefix(nc.NICSelector, "by-name:"):
		nm := strings.TrimPrefix(nc.NICSelector, "by-name:")
		if isValidMAC(nm) {
			mac = strings.ToLower(nm)
		}
	}
	if mac != "" {
		for i, n := range nics {
			if strings.ToLower(n.MAC) == mac {
				ethIdx = i
				break
			}
		}
	}
	dev := fmt.Sprintf("eth%d", ethIdx)

	var lines []string
	lines = append(lines, "# Managed by metalkit installer")
	lines = append(lines, "DEVICE="+dev)
	lines = append(lines, "NAME="+dev)
	if mac != "" {
		lines = append(lines, "HWADDR="+strings.ToUpper(mac))
	}
	lines = append(lines, "ONBOOT=yes")
	lines = append(lines, "TYPE=Ethernet")

	if nc.Method == "dhcp" {
		lines = append(lines, "BOOTPROTO=dhcp")
	} else {
		lines = append(lines, "BOOTPROTO=none")
		if b.StaticAddress != "" {
			lines = append(lines, "IPADDR="+b.StaticAddress)
		}
		if nc.PrefixLen > 0 {
			lines = append(lines, fmt.Sprintf("PREFIX=%d", nc.PrefixLen))
		}
		if nc.Gateway != "" {
			lines = append(lines, "GATEWAY="+nc.Gateway)
		}
		for i, dns := range nc.DNS {
			lines = append(lines, fmt.Sprintf("DNS%d=%s", i+1, dns))
		}
	}

	content := strings.Join(lines, "\n") + "\n"
	_ = deps.FS.WriteFile(filepath.Join(sysDir, "ifcfg-"+dev), []byte(content), 0o644)
}

// writeIfcfgBond writes RHEL 7-style ifcfg files for a bond master and its
// slaves into /etc/sysconfig/network-scripts/. Layout:
//
//   - ifcfg-bond0 — bond master, holds the IP / gateway / DNS / BONDING_OPTS.
//   - ifcfg-eth0, ifcfg-eth1, ... — one per slave, MASTER=bond0 SLAVE=yes,
//     HWADDR matches the slave's MAC so the kernel binds the right NIC even
//     when its predictable name differs between live and installed systems.
//
// VLAN is not supported in the ifcfg fallback (only in the cloud-init v2
// path); if cloud-init's v2 rendering succeeds, it will overwrite these
// files anyway. Bond + VLAN on RHEL 7 is rare enough that we accept the
// degraded behavior here.
func writeIfcfgBond(deps Deps, mntRoot string, nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo) {
	bond := nc.Bond
	if bond == nil {
		return
	}
	sysDir := filepath.Join(mntRoot, "etc", "sysconfig", "network-scripts")
	if !deps.FS.Exists(sysDir) {
		_ = deps.FS.MkdirAll(sysDir, 0o755)
	}

	// Build BONDING_OPTS. mode= accepts the textual name on RHEL 7.
	miimon := bond.Miimon
	if miimon == 0 {
		miimon = 100
	}
	opts := []string{
		fmt.Sprintf("mode=%s", bond.Mode),
		fmt.Sprintf("miimon=%d", miimon),
	}
	switch bond.Mode {
	case "active-backup":
		if bond.Primary != "" {
			// Map the user-supplied primary (MAC or name) to the slave's
			// logical ifcfg device name (eth0, eth1, ...).
			for i, s := range bond.Slaves {
				if s == bond.Primary {
					opts = append(opts, fmt.Sprintf("primary=eth%d", i))
					break
				}
			}
		}
	case "802.3ad":
		rate := bond.LACPRate
		if rate == "" {
			rate = "fast"
		}
		opts = append(opts, fmt.Sprintf("lacp_rate=%s", rate))
		policy := bond.XmitHashPolicy
		if policy == "" {
			policy = "layer3+4"
		}
		opts = append(opts, fmt.Sprintf("xmit_hash_policy=%s", policy))
	}

	// Master ifcfg-bond0.
	var master []string
	master = append(master, "# Managed by metalkit installer")
	master = append(master, "DEVICE=bond0")
	master = append(master, "NAME=bond0")
	master = append(master, "TYPE=Bond")
	master = append(master, "BONDING_MASTER=yes")
	master = append(master, "ONBOOT=yes")
	master = append(master, fmt.Sprintf("BONDING_OPTS=%q", strings.Join(opts, " ")))
	if nc.Method == "dhcp" {
		master = append(master, "BOOTPROTO=dhcp")
	} else {
		master = append(master, "BOOTPROTO=none")
		if b.StaticAddress != "" {
			master = append(master, "IPADDR="+b.StaticAddress)
		}
		if nc.PrefixLen > 0 {
			master = append(master, fmt.Sprintf("PREFIX=%d", nc.PrefixLen))
		}
		if nc.Gateway != "" {
			master = append(master, "GATEWAY="+nc.Gateway)
		}
		for i, dns := range nc.DNS {
			master = append(master, fmt.Sprintf("DNS%d=%s", i+1, dns))
		}
	}
	_ = deps.FS.WriteFile(
		filepath.Join(sysDir, "ifcfg-bond0"),
		[]byte(strings.Join(master, "\n")+"\n"),
		0o644,
	)

	// One ifcfg per slave, logical names eth0, eth1, ...
	for i, slave := range bond.Slaves {
		dev := fmt.Sprintf("eth%d", i)
		var lines []string
		lines = append(lines, "# Managed by metalkit installer")
		lines = append(lines, "DEVICE="+dev)
		lines = append(lines, "NAME="+dev)
		lines = append(lines, "TYPE=Ethernet")
		if mac := lookupSlaveMAC(slave, nics); isValidMAC(mac) {
			lines = append(lines, "HWADDR="+strings.ToUpper(mac))
		}
		lines = append(lines, "ONBOOT=yes")
		lines = append(lines, "BOOTPROTO=none")
		lines = append(lines, "MASTER=bond0")
		lines = append(lines, "SLAVE=yes")
		_ = deps.FS.WriteFile(
			filepath.Join(sysDir, "ifcfg-"+dev),
			[]byte(strings.Join(lines, "\n")+"\n"),
			0o644,
		)
	}
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

// hasCloudInit reports whether the target rootfs has cloud-init installed.
// Checked by looking for /usr/bin/cloud-init or /usr/sbin/cloud-init.
func hasCloudInit(deps Deps, mntRoot string) bool {
	for _, p := range []string{
		filepath.Join(mntRoot, "usr", "bin", "cloud-init"),
		filepath.Join(mntRoot, "usr", "sbin", "cloud-init"),
		filepath.Join(mntRoot, "usr", "local", "bin", "cloud-init"),
	} {
		if deps.FS.Exists(p) {
			return true
		}
	}
	return false
}

// writeDirectHostname writes /etc/hostname directly into the rootfs.
// This is a fallback for images without cloud-init.
func writeDirectHostname(deps Deps, mntRoot, hostname string) {
	if hostname == "" {
		return
	}
	path := filepath.Join(mntRoot, "etc", "hostname")
	content := hostname + "\n"
	if err := deps.FS.WriteFile(path, []byte(content), 0o644); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("direct-write: failed to write /etc/hostname", "err", err)
		}
	} else if deps.Logger != nil {
		deps.Logger.Info("direct-write: set hostname", "hostname", hostname)
	}
}

// writeDirectPassword sets the root password hash directly in /etc/shadow.
// This is a fallback for images without cloud-init.
func writeDirectPassword(deps Deps, mntRoot, passwordHash string) {
	if passwordHash == "" {
		return
	}
	shadowPath := filepath.Join(mntRoot, "etc", "shadow")
	data, err := deps.FS.ReadFile(shadowPath)
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("direct-write: cannot read /etc/shadow", "err", err)
		}
		return
	}
	lines := strings.Split(string(data), "\n")
	found := false
	for i, line := range lines {
		fields := strings.SplitN(line, ":", 9)
		if len(fields) >= 2 && fields[0] == "root" {
			fields[1] = passwordHash
			lines[i] = strings.Join(fields, ":")
			found = true
			break
		}
	}
	if !found {
		if deps.Logger != nil {
			deps.Logger.Warn("direct-write: no root entry in /etc/shadow")
		}
		return
	}
	if err := deps.FS.WriteFile(shadowPath, []byte(strings.Join(lines, "\n")), 0o640); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("direct-write: failed to write /etc/shadow", "err", err)
		}
	} else if deps.Logger != nil {
		deps.Logger.Info("direct-write: set root password in /etc/shadow")
	}
}

// writeDirectSSHDConfig writes the sshd drop-in config directly.
// This is a fallback for images without cloud-init.
func writeDirectSSHDConfig(deps Deps, mntRoot string) {
	sshdDir := filepath.Join(mntRoot, "etc", "ssh", "sshd_config.d")
	if !deps.FS.Exists(sshdDir) {
		_ = deps.FS.MkdirAll(sshdDir, 0o755)
	}
	path := filepath.Join(sshdDir, "99-metalkit.conf")
	content := "# Managed by metalkit installer\nPermitRootLogin yes\nPasswordAuthentication yes\n"
	if err := deps.FS.WriteFile(path, []byte(content), 0o644); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("direct-write: failed to write sshd config", "err", err)
		}
	} else if deps.Logger != nil {
		deps.Logger.Info("direct-write: wrote sshd drop-in config")
	}
}

// writeDirectNetworkConfig writes network configuration files directly
// into the rootfs based on the resolved renderer. This is a fallback
// for images without cloud-init.
func writeDirectNetworkConfig(deps Deps, spec jobs.InstallSpec, mntRoot string) {
	nc := spec.Profile.Network
	b := spec.Binding
	nics := deps.NICs
	renderer := resolveRenderer(spec.NetworkRenderer, deps, mntRoot)

	switch renderer {
	case components.NetworkManagerRenderer:
		// Write NM keyfiles directly.
		if nc.Bond != nil {
			writeNMKeyfileBond(deps, mntRoot, nc, b, nics)
		} else {
			writeNMKeyfile(deps, mntRoot, nc, b, nics)
		}
	case components.SysconfigRenderer:
		// Already handled by writeIfcfg/writeIfcfgBond — those write
		// directly regardless of cloud-init presence. But double-check
		// in case the renderer was resolved differently.
		if nc.Bond != nil {
			writeIfcfgBond(deps, mntRoot, nc, b, nics)
		} else {
			writeIfcfg(deps, mntRoot, nc, b, nics)
		}
	case components.WickedRenderer:
		writeWickedConfig(deps, spec, mntRoot)
	case components.ENIRenderer:
		writeENIRendererConfig(deps, spec, mntRoot)
	case components.NetPlanRenderer:
		// Write netplan YAML directly as fallback.
		writeDirectNetplan(deps, mntRoot, nc, b, nics)
	}
}

// writeNMKeyfile writes a NetworkManager keyfile for a single NIC directly
// into /etc/NetworkManager/system-connections/.
func writeNMKeyfile(deps Deps, mntRoot string, nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo) {
	nmDir := filepath.Join(mntRoot, "etc", "NetworkManager", "system-connections")
	if !deps.FS.Exists(nmDir) {
		_ = deps.FS.MkdirAll(nmDir, 0o755)
	}

	// Resolve the interface name.
	dev, mac := resolveNICForNM(nc.NICSelector, nics)

	var lines []string
	lines = append(lines, "# Managed by metalkit installer")
	lines = append(lines, "[connection]")
	lines = append(lines, "id=metalkit-"+dev)
	lines = append(lines, "type=ethernet")
	// Do not set interface-name: the live NIC name may differ from the
	// installed OS name. Rely on mac-address matching instead.
	lines = append(lines, "autoconnect=true")
	lines = append(lines, "")
	lines = append(lines, "[ethernet]")
	if mac != "" {
		lines = append(lines, "mac-address="+strings.ToLower(mac))
	}
	lines = append(lines, "")
	lines = append(lines, "[ipv4]")
	if nc.Method == "dhcp" {
		lines = append(lines, "method=auto")
	} else {
		lines = append(lines, "method=manual")
		if b.StaticAddress != "" {
			lines = append(lines, fmt.Sprintf("address1=%s/%d", b.StaticAddress, nc.PrefixLen))
		}
		if nc.Gateway != "" {
			lines = append(lines, "gateway="+nc.Gateway)
		}
		if len(nc.DNS) > 0 {
			lines = append(lines, "dns="+strings.Join(nc.DNS, ";"))
		}
	}

	content := strings.Join(lines, "\n") + "\n"
	filename := "metalkit-" + dev + ".nmconnection"
	if err := deps.FS.WriteFile(filepath.Join(nmDir, filename), []byte(content), 0o600); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("direct-write: failed to write NM keyfile", "err", err)
		}
	} else if deps.Logger != nil {
		deps.Logger.Info("direct-write: wrote NM keyfile", "file", filename)
	}
}

// writeNMKeyfileBond writes NetworkManager keyfiles for a bond and its
// slave interfaces directly into /etc/NetworkManager/system-connections/.
func writeNMKeyfileBond(deps Deps, mntRoot string, nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo) {
	bond := nc.Bond
	if bond == nil {
		return
	}
	nmDir := filepath.Join(mntRoot, "etc", "NetworkManager", "system-connections")
	if !deps.FS.Exists(nmDir) {
		_ = deps.FS.MkdirAll(nmDir, 0o755)
	}

	miimon := bond.Miimon
	if miimon == 0 {
		miimon = 100
	}

	// Write one keyfile per slave.
	// Do NOT set interface-name: the live system's NIC names (eno1, enp1s0f0,
	// etc.) may differ from the installed OS names (eth0, eth1, etc.) due to
	// net.ifnames / biosdevname kernel parameters. Rely solely on mac-address
	// matching so NM finds the right device regardless of its name.
	for i, slave := range bond.Slaves {
		_, mac := resolveSlaveDevAndMAC(slave, nics)

		var lines []string
		lines = append(lines, "# Managed by metalkit installer")
		lines = append(lines, "[connection]")
		lines = append(lines, fmt.Sprintf("id=bond0-slave-%d", i))
		lines = append(lines, "type=ethernet")
		lines = append(lines, "master=bond0")
		lines = append(lines, "slave-type=bond")
		lines = append(lines, "")
		lines = append(lines, "[ethernet]")
		if mac != "" {
			lines = append(lines, "mac-address="+strings.ToLower(mac))
		}

		content := strings.Join(lines, "\n") + "\n"
		filename := fmt.Sprintf("bond0-slave-%d.nmconnection", i)
		_ = deps.FS.WriteFile(filepath.Join(nmDir, filename), []byte(content), 0o600)
	}

	// Write bond master keyfile.
	var master []string
	master = append(master, "# Managed by metalkit installer")
	master = append(master, "[connection]")
	master = append(master, "id=bond0")
	master = append(master, "type=bond")
	master = append(master, "interface-name=bond0")
	master = append(master, "autoconnect=true")
	master = append(master, "")
	master = append(master, "[bond]")
	master = append(master, fmt.Sprintf("miimon=%d", miimon))
	master = append(master, "mode="+bond.Mode)
	switch bond.Mode {
	case "active-backup":
		if bond.Primary != "" {
			primaryDev, _ := resolveSlaveDevAndMAC(bond.Primary, nics)
			master = append(master, "primary="+primaryDev)
		}
	case "802.3ad":
		rate := bond.LACPRate
		if rate == "" {
			rate = "fast"
		}
		master = append(master, "lacp-rate="+rate)
		policy := bond.XmitHashPolicy
		if policy == "" {
			policy = "layer3+4"
		}
		master = append(master, "transmit-hash-policy="+policy)
	}
	master = append(master, "")
	master = append(master, "[ipv4]")
	if nc.Method == "dhcp" {
		master = append(master, "method=auto")
	} else {
		master = append(master, "method=manual")
		if b.StaticAddress != "" {
			master = append(master, fmt.Sprintf("address1=%s/%d", b.StaticAddress, nc.PrefixLen))
		}
		if nc.Gateway != "" {
			master = append(master, "gateway="+nc.Gateway)
		}
		if len(nc.DNS) > 0 {
			master = append(master, "dns="+strings.Join(nc.DNS, ";"))
		}
	}

	content := strings.Join(master, "\n") + "\n"
	if err := deps.FS.WriteFile(filepath.Join(nmDir, "bond0.nmconnection"), []byte(content), 0o600); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("direct-write: failed to write bond NM keyfile", "err", err)
		}
	} else if deps.Logger != nil {
		deps.Logger.Info("direct-write: wrote bond0 NM keyfile")
	}

	// VLAN on top of bond.
	if nc.VLAN > 0 {
		writeNMKeyfileVLAN(deps, mntRoot, nc, b, nmDir)
	}
}

// writeNMKeyfileVLAN writes a NetworkManager keyfile for a VLAN interface.
func writeNMKeyfileVLAN(deps Deps, mntRoot string, nc profiles.NetworkConfig, b bindings.Binding, nmDir string) {
	parent := "bond0"
	if nc.Bond == nil {
		parent = "metalkit0"
	}

	var lines []string
	lines = append(lines, "# Managed by metalkit installer")
	lines = append(lines, "[connection]")
	lines = append(lines, fmt.Sprintf("id=%s.%d", parent, nc.VLAN))
	lines = append(lines, "type=vlan")
	lines = append(lines, fmt.Sprintf("interface-name=%s.%d", parent, nc.VLAN))
	lines = append(lines, "autoconnect=true")
	lines = append(lines, "")
	lines = append(lines, "[vlan]")
	lines = append(lines, fmt.Sprintf("parent=%s", parent))
	lines = append(lines, fmt.Sprintf("id=%d", nc.VLAN))
	lines = append(lines, "")
	lines = append(lines, "[ipv4]")
	if nc.Method == "dhcp" {
		lines = append(lines, "method=auto")
	} else {
		lines = append(lines, "method=manual")
		if b.StaticAddress != "" {
			lines = append(lines, fmt.Sprintf("address1=%s/%d", b.StaticAddress, nc.PrefixLen))
		}
		if nc.Gateway != "" {
			lines = append(lines, "gateway="+nc.Gateway)
		}
		if len(nc.DNS) > 0 {
			lines = append(lines, "dns="+strings.Join(nc.DNS, ";"))
		}
	}

	filename := fmt.Sprintf("%s.%d.nmconnection", parent, nc.VLAN)
	_ = deps.FS.WriteFile(filepath.Join(nmDir, filename), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// writeDirectNetplan writes a netplan YAML file directly as a fallback
// when cloud-init is absent on netplan-based distros.
func writeDirectNetplan(deps Deps, mntRoot string, nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo) {
	netplanDir := filepath.Join(mntRoot, "etc", "netplan")
	if !deps.FS.Exists(netplanDir) {
		_ = deps.FS.MkdirAll(netplanDir, 0o755)
	}

	rhel7 := isRHEL7Root(deps, mntRoot)
	netCfg := renderNetworkConfig(nc, b, nics, rhel7)
	content := "network:\n" + indentYAML(netCfg)

	path := filepath.Join(netplanDir, "50-metalkit.yaml")
	if err := deps.FS.WriteFile(path, []byte(content), 0o600); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("direct-write: failed to write netplan config", "err", err)
		}
	} else if deps.Logger != nil {
		deps.Logger.Info("direct-write: wrote netplan config", "path", path)
	}
}

// indentYAML adds a 2-space indent to every line of the input for nesting
// under a `network:` key.
func indentYAML(s string) string {
	var out strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out.WriteString("  ")
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// resolveNICForNM returns the interface name and MAC address for a given
// NIC selector, suitable for writing into a NetworkManager keyfile.
func resolveNICForNM(selector string, nics []NICInfo) (name, mac string) {
	switch {
	case strings.HasPrefix(selector, "by-mac:"):
		mac = strings.ToLower(strings.TrimPrefix(selector, "by-mac:"))
		for _, n := range nics {
			if strings.ToLower(n.MAC) == mac && n.Name != "" {
				name = n.Name
				return
			}
		}
		name = "eth0"
	case strings.HasPrefix(selector, "by-name:"):
		nm := strings.TrimPrefix(selector, "by-name:")
		if isValidMAC(nm) {
			mac = strings.ToLower(nm)
			for _, n := range nics {
				if strings.EqualFold(n.MAC, nm) && n.Name != "" {
					name = n.Name
					return
				}
			}
			name = "eth0"
		} else if nm != "" {
			name = nm
			for _, n := range nics {
				if n.Name == nm {
					mac = strings.ToLower(n.MAC)
					break
				}
			}
		} else {
			name = "eth0"
		}
	default:
		// auto: use first NIC with a name.
		name = "eth0"
		for _, n := range nics {
			if n.Name != "" {
				name = n.Name
				mac = strings.ToLower(n.MAC)
				break
			}
		}
	}
	return
}

// resolveSlaveDevAndMAC resolves a bond slave specifier (which may be a MAC
// address, a by-name:IFNAME selector, or a plain interface name) into the
// kernel-visible device name and its MAC address. Falls back to the input
// value if resolution fails.
func resolveSlaveDevAndMAC(slave string, nics []NICInfo) (dev, mac string) {
	if isValidMAC(slave) {
		mac = strings.ToLower(slave)
		for _, n := range nics {
			if strings.EqualFold(n.MAC, slave) && n.Name != "" {
				dev = n.Name
				return
			}
		}
		dev = slave
		return
	}
	// Handle by-name: prefix.
	name := slave
	if strings.HasPrefix(slave, "by-name:") {
		name = strings.TrimPrefix(slave, "by-name:")
	}
	mac = resolveNICMAC(nics, name)
	if mac == "" {
		// Try resolving name as a MAC.
		if isValidMAC(name) {
			mac = strings.ToLower(name)
			for _, n := range nics {
				if strings.EqualFold(n.MAC, name) && n.Name != "" {
					dev = n.Name
					return
				}
			}
		}
	}
	dev = name
	return
}
