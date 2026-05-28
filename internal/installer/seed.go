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

	// Always write the network-config so cloud-init has a v2 config to
	// process and does not fall back to DHCP. For RHEL7 rootfs, cloud-init's
	// sysconfig renderer turns the netplan YAML key into the ifcfg DEVICE=
	// field — and CentOS 7 boots with net.ifnames=0, so the kernel will
	// never produce an "eno2" device. Pass rhel7=true so the renderer falls
	// back to "ethN" matching the picked NIC's PCI position.
	rhel7 := isRHEL7Root(deps, mntRoot)
	netCfg := renderNetworkConfig(spec.Profile.Network, spec.Binding, deps.NICs, rhel7)
	netPath := filepath.Join(seedDir, "network-config")
	if err := deps.FS.WriteFile(netPath, []byte(netCfg), 0o644); err != nil {
		return fmt.Errorf("install: write network-config: %w", err)
	}

	// CentOS 7 / RHEL 7 cloud-init 19.4 may not fully support v2. Write
	// the ifcfg files directly as a safety net — they will be overwritten
	// by cloud-init if the v2 render succeeds, but remain if it fails.
	if rhel7 {
		if spec.Profile.Network.Bond == nil {
			writeIfcfg(deps, mntRoot, spec.Profile.Network, spec.Binding, deps.NICs)
		} else {
			writeIfcfgBond(deps, mntRoot, spec.Profile.Network, spec.Binding, deps.NICs)
		}
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

	// Pin cloud-init's network renderer to NetworkManager on modern
	// RHEL family (Rocky 9 / AlmaLinux / RHEL 8+ / Fedora). cloud-init's
	// default renderer priority for "rhel" distro is
	//   [sysconfig, eni, netplan, freebsd, networkd]
	// so without this override cloud-init writes legacy ifcfg files into
	// /etc/sysconfig/network-scripts/, but the installed OS uses
	// NetworkManager for live management — the ifcfg files only work via
	// NM's deprecated ifcfg-rh compatibility plugin and don't show up in
	// `nmcli con show`. Switching to the `network-manager` renderer makes
	// cloud-init emit native `.nmconnection` keyfiles, which is what
	// operators expect on a Rocky/RHEL system.
	//
	// CentOS 7 stays on sysconfig (its cloud-init 19.4 has no NM renderer);
	// Ubuntu/Debian don't have RHEL renderer ordering issues.
	if prefersNetworkManagerRenderer(deps, mntRoot) {
		rendererPath := filepath.Join(dsDir, "98-metalkit-renderer.cfg")
		rendererContent := "# Managed by metalkit installer — prefer NetworkManager keyfiles over legacy ifcfg.\n" +
			"system_info:\n" +
			"  network:\n" +
			"    renderers: [ network-manager, sysconfig ]\n"
		if err := deps.FS.WriteFile(rendererPath, []byte(rendererContent), 0o644); err != nil {
			return fmt.Errorf("install: write renderer config: %w", err)
		}
	}

	// RHEL family (CentOS / RHEL / Rocky / AlmaLinux / Fedora) ships with
	// SELinux in enforcing mode. Files we wrote into the rootfs from the
	// live system (ifcfg-*, /var/lib/cloud/seed/*, /etc/cloud/cloud.cfg.d/*)
	// inherit the mount-time context (typically default_t / unlabeled_t),
	// which is wrong for net_conf_t / cloud_init_t. Without relabel, first
	// boot drops to emergency mode (sshd fails to load policy). Touching
	// /.autorelabel triggers a full SELinux relabel on first boot, after
	// which the system reboots automatically. Harmless on systems without
	// SELinux.
	if isRHELFamilyRoot(deps, mntRoot) {
		relabelPath := filepath.Join(mntRoot, ".autorelabel")
		if err := deps.FS.WriteFile(relabelPath, nil, 0o644); err != nil {
			return fmt.Errorf("install: touch /.autorelabel: %w", err)
		}
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
		return renderNetworkConfigBond(nc, b, nics)
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

func renderNetworkConfigBond(nc profiles.NetworkConfig, b bindings.Binding, nics []NICInfo) string {
	bond := nc.Bond
	var sb strings.Builder
	sb.WriteString("version: 2\n")

	// Use logical names (slave-0, slave-1, ...) instead of live NIC names
	// as YAML keys. Live NIC names (eno1/eno2) may differ from installed OS
	// names (eth0/eth1 on CentOS 7 with net.ifnames=0). Logical names + MAC
	// matching (match.macaddress) let NetworkManager/netplan bind the config
	// to the correct physical NIC regardless of kernel naming.
	sb.WriteString("ethernets:\n")
	logicalNames := make([]string, len(bond.Slaves))
	for i, slave := range bond.Slaves {
		ln := fmt.Sprintf("slave-%d", i)
		logicalNames[i] = ln
		fmt.Fprintf(&sb, "  %s:\n", ln)
		sb.WriteString("    match:\n")
		if mac := lookupSlaveMAC(slave, nics); isValidMAC(mac) {
			fmt.Fprintf(&sb, "      macaddress: %q\n", strings.ToLower(mac))
		} else {
			fmt.Fprintf(&sb, "      name: %q\n", slave)
		}
	}
	sb.WriteString("bonds:\n")
	sb.WriteString("  bond0:\n")
	sb.WriteString("    interfaces: [")
	for i, n := range logicalNames {
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
					fmt.Fprintf(&sb, "      primary: %s\n", logicalNames[i])
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
}

// isRHELFamilyRoot detects any RHEL-family distribution (CentOS, RHEL,
// Rocky, AlmaLinux, Fedora) from /etc/os-release inside the mounted rootfs.
// Used to decide whether SELinux autorelabel is needed on first boot — all
// RHEL-family distros ship with SELinux in enforcing mode.
func isRHELFamilyRoot(deps Deps, mntRoot string) bool {
	data, err := deps.FS.ReadFile(filepath.Join(mntRoot, "etc", "os-release"))
	if err != nil {
		return false
	}
	c := string(data)
	for _, marker := range []string{
		`ID="centos"`,
		`ID="rhel"`,
		`ID="rocky"`,
		`ID="almalinux"`,
		`ID="fedora"`,
		`ID_LIKE="rhel`,
		`ID_LIKE="fedora`,
		`ID_LIKE=rhel`,
		`ID_LIKE=fedora`,
	} {
		if strings.Contains(c, marker) {
			return true
		}
	}
	return false
}

// prefersNetworkManagerRenderer reports whether the target rootfs should
// have cloud-init emit NetworkManager keyfiles instead of legacy ifcfg.
// True for modern RHEL family (Rocky / AlmaLinux / RHEL 8+ / Fedora);
// false for CentOS 7 / RHEL 7 (no NM renderer in their cloud-init) and
// for non-RHEL distros (Ubuntu uses netplan natively).
func prefersNetworkManagerRenderer(deps Deps, mntRoot string) bool {
	if !isRHELFamilyRoot(deps, mntRoot) {
		return false
	}
	if isRHEL7Root(deps, mntRoot) {
		return false
	}
	return true
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
