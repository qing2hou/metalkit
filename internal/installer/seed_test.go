package installer

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"metalkit/internal/bindings"
	"metalkit/internal/jobs"
	"metalkit/internal/profiles"
)

func TestExpandHostname(t *testing.T) {
	cases := []struct {
		name     string
		template string
		binding  bindings.Binding
		want     string
	}{
		{
			name:     "binding hostname overrides template",
			template: "host-{serial}",
			binding:  bindings.Binding{Hostname: "fixed-name"},
			want:     "fixed-name",
		},
		{
			name:     "uuid8 substitution",
			template: "node-{uuid8}",
			binding:  bindings.Binding{MachineUUID: "01234567-89ab-cdef-0123-456789abcdef"},
			want:     "node-01234567",
		},
		{
			name:     "serial maps to first 8 chars of uuid (M2.3 stand-in)",
			template: "srv-{serial}",
			binding:  bindings.Binding{MachineUUID: "deadbeef-0000-0000-0000-000000000000"},
			want:     "srv-deadbeef",
		},
		{
			name:     "mac left literal (documented)",
			template: "host-{mac}",
			binding:  bindings.Binding{MachineUUID: "01234567xxx"},
			want:     "host-{mac}",
		},
		{
			name:     "no placeholder leaves template alone",
			template: "constant.local",
			binding:  bindings.Binding{MachineUUID: "01234567"},
			want:     "constant.local",
		},
		{
			name:     "short uuid uses entire string",
			template: "n-{uuid8}",
			binding:  bindings.Binding{MachineUUID: "abc"},
			want:     "n-abc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandHostname(tc.template, tc.binding, profiles.Profile{})
			if got != tc.want {
				t.Fatalf("expandHostname: got %q want %q", got, tc.want)
			}
		})
	}
}

func TestRenderUserData_DHCP(t *testing.T) {
	spec := jobs.InstallSpec{
		JobID:       "job-aaaa-bbbb-cccc-dddd",
		MachineUUID: "deadbeef00000000",
		Profile: profiles.Profile{OSFamily: "ubuntu",
			HostnameTemplate: "n-{uuid8}",
			RootPasswordHash: "$6$salt$" + strings.Repeat("a", 86),
			Network:          profiles.NetworkConfig{Method: "dhcp", NICSelector: "auto"},
		},
		Binding: bindings.Binding{
			MachineUUID: "deadbeef00000000",
		},
	}
	got := renderUserData(spec, expandHostname(spec.Profile.HostnameTemplate, spec.Binding, spec.Profile))

	mustContain := []string{
		"#cloud-config",
		`hostname: "n-deadbeef"`,
		"disable_root: false",
		"ssh_pwauth: true",
		"chpasswd:",
		"  expire: false",
		"- name: root",
		"      type: hash",
		// sshd drop-in is emitted unconditionally so root password login works.
		"write_files:",
		"  - path: /etc/ssh/sshd_config.d/99-metalkit.conf",
		"      PermitRootLogin yes",
		"      PasswordAuthentication yes",
		"runcmd:",
		"- [usermod",
		"systemctl restart sshd",
		"systemctl restart ssh",
		"|| true",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Fatalf("dhcp user-data missing %q\nGOT:\n%s", sub, got)
		}
	}
	// netplan-related content must NOT appear in DHCP mode.
	if strings.Contains(got, "netplan") {
		t.Fatalf("dhcp user-data must NOT contain netplan refs\nGOT:\n%s", got)
	}
	if strings.Contains(got, "/etc/netplan/01-metalkit.yaml") {
		t.Fatalf("dhcp user-data must NOT write netplan file\nGOT:\n%s", got)
	}
}

func TestRenderUserData_Static(t *testing.T) {
	spec := jobs.InstallSpec{
		JobID: "job-static",
		Profile: profiles.Profile{OSFamily: "ubuntu",
			HostnameTemplate: "host-{uuid8}",
			RootPasswordHash: "$6$abcd$" + strings.Repeat("b", 86),
			Network: profiles.NetworkConfig{
				Method:      "static",
				PrefixLen:   24,
				Gateway:     "10.0.0.1",
				DNS:         []string{"8.8.8.8", "1.1.1.1"},
				NICSelector: "by-mac:aa:bb:cc:dd:ee:ff",
			},
		},
		Binding: bindings.Binding{
			MachineUUID:   "abcd1234deadbeef",
			StaticAddress: "10.0.0.50",
		},
	}
	got := renderUserData(spec, expandHostname(spec.Profile.HostnameTemplate, spec.Binding, spec.Profile))
	mustContain := []string{
		"#cloud-config",
		`hostname: "host-abcd1234"`,
		"disable_root: false",
		// sshd drop-in
		"  - path: /etc/ssh/sshd_config.d/99-metalkit.conf",
		"      PermitRootLogin yes",
		"      PasswordAuthentication yes",
		"runcmd:",
		"- [usermod",
		"systemctl restart sshd",
		"systemctl restart ssh",
		"|| true",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Fatalf("static user-data missing %q\nGOT:\n%s", sub, got)
		}
	}
	// netplan content must NOT be embedded in user-data anymore — it lives
	// in the NoCloud network-config file. This guards against regression
	// (we used to ship two conflicting netplan files; see seed.go).
	mustNotContain := []string{
		"netplan",
		"/etc/netplan/",
		"01-metalkit.yaml",
	}
	for _, sub := range mustNotContain {
		if strings.Contains(got, sub) {
			t.Fatalf("static user-data must NOT contain %q\nGOT:\n%s", sub, got)
		}
	}
}

func TestRenderNetworkConfig_StaticByMAC(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "10.0.0.1",
		DNS:         []string{"8.8.8.8", "1.1.1.1"},
		NICSelector: "by-mac:AA:BB:CC:DD:EE:FF",
	}
	b := bindings.Binding{StaticAddress: "10.0.0.50"}
	got := renderNetworkConfig(nc, b, nil, false)

	mustContain := []string{
		"version: 2",
		"ethernets:",
		"  eth0:",
		// MAC is lower-cased for netplan compatibility.
		`      macaddress: "aa:bb:cc:dd:ee:ff"`,
		"    dhcp4: false",
		`    addresses: ["10.0.0.50/24"]`,
		`    gateway4: "10.0.0.1"`,
		`      addresses: ["8.8.8.8", "1.1.1.1"]`,
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Fatalf("network-config missing %q\nGOT:\n%s", sub, got)
		}
	}
	// NoCloud's network-config format has NO outer `network:` wrapper —
	// only the netplan-style file written to /etc/netplan does.
	if strings.HasPrefix(got, "network:") {
		t.Fatalf("network-config must not start with `network:` wrapper\nGOT:\n%s", got)
	}
}

// When the agent reports live NICs and the selector points at one of them
// (by-mac), the YAML key should be the kernel-visible name (eno2) instead of
// the generic placeholder eth0. The match.macaddress still pins the binding
// physically — the key change is cosmetic but lines up `cat /etc/netplan/...`
// with what the operator sees in `ip a`.
func TestRenderNetworkConfig_KeyIsLiveNICName(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "192.168.10.1",
		NICSelector: "by-mac:24:6e:96:4f:f8:f1",
	}
	b := bindings.Binding{StaticAddress: "192.168.10.192"}
	nics := []NICInfo{
		{Name: "eno1", MAC: "24:6e:96:4f:f8:f0"},
		{Name: "eno2", MAC: "24:6e:96:4f:f8:f1"},
		{Name: "eno3", MAC: "24:6e:96:4f:f8:f2"},
	}
	got := renderNetworkConfig(nc, b, nics, false)
	if !strings.Contains(got, "  eno2:") {
		t.Fatalf("expected YAML key `  eno2:` (live NIC name), got:\n%s", got)
	}
	if strings.Contains(got, "  eth0:") {
		t.Fatalf("did not expect generic `eth0` key when live NIC resolves:\n%s", got)
	}
	if !strings.Contains(got, `macaddress: "24:6e:96:4f:f8:f1"`) {
		t.Fatalf("expected MAC matcher preserved:\n%s", got)
	}
}

// CentOS 7 / RHEL 7 cloud images boot with net.ifnames=0 on the kernel
// cmdline — the installed OS sees eth0/eth1/... regardless of the live
// agent's eno2/ens1np0 names. The netplan YAML key (which cloud-init's
// sysconfig renderer pipes into ifcfg's DEVICE=) must use eth<N> matching
// the picked NIC's index in the live list, NOT the live name.
func TestRenderNetworkConfig_RHEL7UsesEthIndex(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "192.168.10.1",
		NICSelector: "by-mac:24:6e:96:4f:f8:f1",
	}
	b := bindings.Binding{StaticAddress: "192.168.10.192"}
	nics := []NICInfo{
		{Name: "eno1", MAC: "24:6e:96:4f:f8:f0"},
		{Name: "eno2", MAC: "24:6e:96:4f:f8:f1"}, // picked NIC: index 1 → eth1
		{Name: "eno3", MAC: "24:6e:96:4f:f8:f2"},
	}
	got := renderNetworkConfig(nc, b, nics, true)
	if !strings.Contains(got, "  eth1:") {
		t.Fatalf("rhel7 mode: expected key `  eth1:` (index 1), got:\n%s", got)
	}
	if strings.Contains(got, "  eno2:") {
		t.Fatalf("rhel7 mode: must NOT use live name eno2 (kernel will rename to ethN):\n%s", got)
	}
}

// systemd predictable interface names cover several stylistic flavours
// (eno2, ens3, ens1np0 for Mellanox/Broadcom multi-port cards, enp1s0f1
// for legacy enumeration). The resolver must be naming-style agnostic —
// it returns whatever the agent saw in netlink. This test pins that
// behavior so a future regex tightening doesn't accidentally reject one.
func TestRenderNetworkConfig_KeyAgnosticToNICNamingStyle(t *testing.T) {
	cases := []struct {
		name string
		nic  NICInfo
	}{
		{"eno2 (legacy onboard)", NICInfo{Name: "eno2", MAC: "24:6e:96:4f:f8:f1"}},
		{"ens3 (slot+function)", NICInfo{Name: "ens3", MAC: "52:54:00:12:34:56"}},
		{"ens1np0 (Mellanox port)", NICInfo{Name: "ens1np0", MAC: "b8:ce:f6:11:22:33"}},
		{"enp1s0f1 (BIOS path)", NICInfo{Name: "enp1s0f1", MAC: "ac:1f:6b:aa:bb:cc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := profiles.NetworkConfig{
				Method:      "static",
				PrefixLen:   24,
				Gateway:     "10.0.0.1",
				NICSelector: "by-mac:" + tc.nic.MAC,
			}
			b := bindings.Binding{StaticAddress: "10.0.0.50"}
			got := renderNetworkConfig(nc, b, []NICInfo{tc.nic}, false)
			want := "  " + tc.nic.Name + ":"
			if !strings.Contains(got, want) {
				t.Fatalf("expected key %q in output:\n%s", want, got)
			}
		})
	}
}

func TestRenderNetworkConfig_ByName(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "192.168.1.1",
		NICSelector: "by-name:eno1",
	}
	b := bindings.Binding{StaticAddress: "192.168.1.10"}
	got := renderNetworkConfig(nc, b, nil, false)
	if !strings.Contains(got, `name: "eno1"`) {
		t.Fatalf("by-name selector missing name match:\n%s", got)
	}
	if !strings.Contains(got, `addresses: ["192.168.1.10/24"]`) {
		t.Fatalf("static address missing:\n%s", got)
	}
}

func TestRenderNetworkConfig_Auto(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "10.0.0.1",
		NICSelector: "auto",
	}
	b := bindings.Binding{StaticAddress: "10.0.0.5"}
	got := renderNetworkConfig(nc, b, nil, false)
	if !strings.Contains(got, `name: "en*"`) {
		t.Fatalf("auto selector must produce en* match:\n%s", got)
	}
}

func TestRenderNetworkConfig_BondActiveBackup(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "10.0.0.1",
		DNS:         []string{"8.8.8.8"},
		NICSelector: "auto",
		Bond: &profiles.BondConfig{
			Mode:    "active-backup",
			Slaves:  []string{"eno1", "eno2"},
			Miimon:  100,
			Primary: "eno1",
		},
	}
	b := bindings.Binding{StaticAddress: "10.0.0.50"}
	// No NICs passed — bond slaves use the interface name as YAML key
	// and fall back to match.name for binding.
	got := renderNetworkConfig(nc, b, nil, false)

	mustContain := []string{
		"version: 2",
		"ethernets:",
		"  eno1:",
		`      name: "eno1"`,
		"  eno2:",
		`      name: "eno2"`,
		"bonds:",
		"  bond0:",
		"    interfaces: [eno1, eno2]",
		"    parameters:",
		"      mode: active-backup",
		"      mii-monitor-interval: 100",
		"      primary: eno1",
		"    dhcp4: false",
		`    addresses: ["10.0.0.50/24"]`,
		`    gateway4: "10.0.0.1"`,
		`      addresses: ["8.8.8.8"]`,
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Fatalf("bond active-backup missing %q\nGOT:\n%s", sub, got)
		}
	}
	// LACP-only keys must NOT appear in active-backup output.
	for _, sub := range []string{"lacp-rate", "transmit-hash-policy"} {
		if strings.Contains(got, sub) {
			t.Fatalf("active-backup leaked %q\nGOT:\n%s", sub, got)
		}
	}
}

func TestRenderNetworkConfig_BondActiveBackup_WithNICs(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "10.0.0.1",
		DNS:         []string{"8.8.8.8"},
		NICSelector: "auto",
		Bond: &profiles.BondConfig{
			Mode:    "active-backup",
			Slaves:  []string{"eno1", "eno2"},
			Miimon:  100,
			Primary: "eno1",
		},
	}
	b := bindings.Binding{StaticAddress: "10.0.0.50"}
	// Live NICs available — bond slaves resolve to real names with match.macaddress.
	nics := []NICInfo{
		{Name: "eno1", MAC: "aa:bb:cc:dd:ee:01"},
		{Name: "eno2", MAC: "aa:bb:cc:dd:ee:02"},
	}
	got := renderNetworkConfig(nc, b, nics, false)

	mustContain := []string{
		"  eno1:",
		`      macaddress: "aa:bb:cc:dd:ee:01"`,
		"  eno2:",
		`      macaddress: "aa:bb:cc:dd:ee:02"`,
		"      primary: eno1",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Fatalf("bond with NICs missing %q\nGOT:\n%s", sub, got)
		}
	}
}

func TestRenderNetworkConfig_BondWithMACSlaves(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "10.0.0.1",
		DNS:         []string{"8.8.8.8"},
		NICSelector: "auto",
		Bond: &profiles.BondConfig{
			Mode:   "802.3ad",
			Slaves: []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"},
		},
	}
	b := bindings.Binding{StaticAddress: "10.0.0.50"}
	got := renderNetworkConfig(nc, b, nil, false)

	mustContain := []string{
		"  eth0:",
		`      macaddress: "aa:bb:cc:dd:ee:01"`,
		"  eth1:",
		`      macaddress: "aa:bb:cc:dd:ee:02"`,
		"    interfaces: [eth0, eth1]",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Fatalf("bond MAC slaves missing %q\nGOT:\n%s", sub, got)
		}
	}
	// No match.name fallback when MAC slaves are used.
	if strings.Contains(got, `name:`) {
		t.Fatalf("MAC-based bond slaves should not produce match.name\nGOT:\n%s", got)
	}
}

func TestRenderNetworkConfig_BondWithMACSlaves_WithNICs(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "10.0.0.1",
		NICSelector: "auto",
		Bond: &profiles.BondConfig{
			Mode:   "active-backup",
			Slaves: []string{"24:6e:96:4f:f8:f0", "24:6e:96:4f:f8:f1"},
		},
	}
	b := bindings.Binding{StaticAddress: "10.0.0.50"}
	nics := []NICInfo{
		{Name: "eth0", MAC: "24:6e:96:4f:f8:f0"},
		{Name: "eth1", MAC: "24:6e:96:4f:f8:f1"},
		{Name: "eth2", MAC: "24:6e:96:4f:f8:f2"},
		{Name: "eth3", MAC: "24:6e:96:4f:f8:f3"},
	}
	got := renderNetworkConfig(nc, b, nics, false)

	mustContain := []string{
		"  eth0:",
		`      macaddress: "24:6e:96:4f:f8:f0"`,
		"  eth1:",
		`      macaddress: "24:6e:96:4f:f8:f1"`,
		"    interfaces: [eth0, eth1]",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Fatalf("bond MAC slaves with NICs missing %q\nGOT:\n%s", sub, got)
		}
	}
}

func TestRenderNetworkConfig_BondWithMACSlaves_RHEL7(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "10.0.0.1",
		NICSelector: "auto",
		Bond: &profiles.BondConfig{
			Mode:   "active-backup",
			Slaves: []string{"24:6e:96:4f:f8:f0", "24:6e:96:4f:f8:f1"},
		},
	}
	b := bindings.Binding{StaticAddress: "10.0.0.50"}
	nics := []NICInfo{
		{Name: "eno1", MAC: "24:6e:96:4f:f8:f0"},
		{Name: "eno2", MAC: "24:6e:96:4f:f8:f1"},
	}
	got := renderNetworkConfig(nc, b, nics, true)

	mustContain := []string{
		"  eth0:",
		`      macaddress: "24:6e:96:4f:f8:f0"`,
		"  eth1:",
		`      macaddress: "24:6e:96:4f:f8:f1"`,
		"    interfaces: [eth0, eth1]",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Fatalf("RHEL7 bond MAC slaves missing %q\nGOT:\n%s", sub, got)
		}
	}
}

func TestRenderNetworkConfig_BondLACP(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "dhcp",
		NICSelector: "auto",
		Bond: &profiles.BondConfig{
			Mode:           "802.3ad",
			Slaves:         []string{"eno1", "eno2"},
			Miimon:         100,
			LACPRate:       "fast",
			XmitHashPolicy: "layer3+4",
		},
	}
	got := renderNetworkConfig(nc, bindings.Binding{}, nil, false)

	mustContain := []string{
		"      mode: 802.3ad",
		"      mii-monitor-interval: 100",
		"      lacp-rate: fast",
		"      transmit-hash-policy: layer3+4",
		"    dhcp4: true",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Fatalf("bond LACP missing %q\nGOT:\n%s", sub, got)
		}
	}
	// active-backup-only key must NOT appear under LACP.
	if strings.Contains(got, "primary:") {
		t.Fatalf("LACP leaked primary:\nGOT:\n%s", got)
	}
	// In bond+dhcp, no static IP fields should appear.
	for _, sub := range []string{"addresses:", "gateway4:", "nameservers:"} {
		if strings.Contains(got, sub) {
			t.Fatalf("bond+dhcp leaked %q\nGOT:\n%s", sub, got)
		}
	}
}

func TestRenderNetworkConfig_DHCP(t *testing.T) {
	nc := profiles.NetworkConfig{Method: "dhcp", NICSelector: "auto"}
	got := renderNetworkConfig(nc, bindings.Binding{}, nil, false)
	if !strings.Contains(got, "dhcp4: true") {
		t.Fatalf("dhcp network-config missing dhcp4: true\nGOT:\n%s", got)
	}
	// no static-only fields
	for _, sub := range []string{"addresses:", "gateway4:", "nameservers:"} {
		if strings.Contains(got, sub) {
			t.Fatalf("dhcp network-config must not contain %q\nGOT:\n%s", sub, got)
		}
	}
}

func TestRenderNetworkConfig_VLAN_Single(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "10.0.0.1",
		DNS:         []string{"8.8.8.8"},
		NICSelector: "by-mac:aa:bb:cc:dd:ee:ff",
		VLAN:        100,
	}
	b := bindings.Binding{StaticAddress: "10.0.0.50"}
	got := renderNetworkConfig(nc, b, nil, false)

	mustContain := []string{
		"ethernets:",
		"  eth0:",
		`      macaddress: "aa:bb:cc:dd:ee:ff"`,
		"vlans:",
		"  eth0.100:",
		"    id: 100",
		"    link: eth0",
		"    dhcp4: false",
		`    addresses: ["10.0.0.50/24"]`,
		`    gateway4: "10.0.0.1"`,
		`      addresses: ["8.8.8.8"]`,
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Fatalf("vlan single missing %q\nGOT:\n%s", sub, got)
		}
	}
}

func TestRenderNetworkConfig_VLAN_Bond(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "dhcp",
		NICSelector: "auto",
		VLAN:        200,
		Bond: &profiles.BondConfig{
			Mode:   "802.3ad",
			Slaves: []string{"eno1", "eno2"},
		},
	}
	got := renderNetworkConfig(nc, bindings.Binding{}, nil, false)

	mustContain := []string{
		"bonds:",
		"  bond0:",
		"vlans:",
		"  bond0.200:",
		"    id: 200",
		"    link: bond0",
		"    dhcp4: true",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Fatalf("vlan bond missing %q\nGOT:\n%s", sub, got)
		}
	}
}

func TestRenderNetworkConfig_DHCP_NoVLAN(t *testing.T) {
	nc := profiles.NetworkConfig{Method: "dhcp", NICSelector: "auto", VLAN: 0}
	got := renderNetworkConfig(nc, bindings.Binding{}, nil, false)
	if !strings.Contains(got, "dhcp4: true") {
		t.Fatalf("dhcp no-vlan missing dhcp4: true\nGOT:\n%s", got)
	}
	if strings.Contains(got, "vlans:") {
		t.Fatalf("dhcp no-vlan leaked vlan section\nGOT:\n%s", got)
	}
}

func TestRenderMetaData(t *testing.T) {
	spec := jobs.InstallSpec{JobID: "0123456789abcdef"}
	got := renderMetaData(spec, "host.example")
	want := "instance-id: metalkit-0123456789ab\nlocal-hostname: \"host.example\"\n"
	if got != want {
		t.Fatalf("meta-data:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestBuildSeed(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	deps := Deps{Exec: exec, FS: fs, WorkDir: "/tmp/test-install"}
	spec := jobs.InstallSpec{
		JobID: "job-x",
		Profile: profiles.Profile{OSFamily: "ubuntu",
			HostnameTemplate: "n",
			RootPasswordHash: "$6$s$" + strings.Repeat("a", 86),
			Network:          profiles.NetworkConfig{Method: "dhcp", NICSelector: "auto"},
		},
		Binding: bindings.Binding{MachineUUID: "abcd1234"},
	}

	if err := BuildSeed(context.Background(), deps, spec, "/mnt/root"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Three seed files written into the rootfs NoCloud seed dir.
	seedDir := "/mnt/root/var/lib/cloud/seed/nocloud-net"
	for _, name := range []string{"user-data", "meta-data", "network-config"} {
		if _, ok := fs.files[seedDir+"/"+name]; !ok {
			t.Fatalf("%s not written under %s", name, seedDir)
		}
	}
	// Datasource config must be written directly into rootfs so
	// cloud-init knows to use the NoCloud seed.
	dsPath := "/mnt/root/etc/cloud/cloud.cfg.d/99-metalkit.cfg"
	ds, ok := fs.files[dsPath]
	if !ok {
		t.Fatalf("datasource config not written at %s", dsPath)
	}
	if !strings.Contains(string(ds), "NoCloud") {
		t.Fatalf("datasource config missing NoCloud: %s", string(ds))
	}
	// network-config must contain dhcp4: true (this spec uses DHCP) and
	// must NOT carry an outer `network:` wrapper (NoCloud format).
	nc := string(fs.files[seedDir+"/network-config"])
	if !strings.Contains(nc, "dhcp4: true") {
		t.Fatalf("network-config missing dhcp4:true\n%s", nc)
	}
	if strings.HasPrefix(nc, "network:") {
		t.Fatalf("network-config must not start with `network:` wrapper\n%s", nc)
	}
	// No external commands (cloud-localds/cp) should be invoked anymore.
	for _, c := range exec.Calls() {
		if c.Name == "cloud-localds" || c.Name == "cp" {
			t.Fatalf("unexpected exec call %s %v — seed should be pure file writes", c.Name, c.Args)
		}
	}
}

func TestBuildSeed_EmptyMntRootErrors(t *testing.T) {
	deps := Deps{Exec: newMockExec(), FS: newMockFS(), WorkDir: "/tmp/t"}
	err := BuildSeed(context.Background(), deps, jobs.InstallSpec{}, "")
	if err == nil {
		t.Fatal("empty mntRoot must error")
	}
}

// ---- isRHELFamilyRoot ------------------------------------------------------

// ---- /.autorelabel on RHEL family ------------------------------------------

func TestBuildSeed_RHELFamily_SELinuxRelabel(t *testing.T) {
	cases := []struct {
		name            string
		osRelease       string
		wantRelabel     bool
		selinuxDisabled bool
	}{
		{name: "centos7", osRelease: `ID="centos"
VERSION_ID="7"`, wantRelabel: true},
		{name: "rocky9", osRelease: `ID="rocky"
VERSION_ID="9.3"`, wantRelabel: true},
		{name: "ubuntu22", osRelease: `ID=ubuntu`, wantRelabel: false},
		{name: "rocky10-selinux-disabled", osRelease: `ID="rocky"
VERSION_ID="10"`, wantRelabel: false, selinuxDisabled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newMockFS()
			_ = fs.MkdirAll("/mnt/root/etc", 0o755)
			_ = fs.MkdirAll("/mnt/root/etc/selinux", 0o755)
			_ = fs.WriteFile("/mnt/root/etc/os-release", []byte(tc.osRelease), 0o644)
			// RHEL-family cloud images ship SELinux=enforcing by default.
			// Without this file, selinuxWillBeDisabled returns true and
			// the restorecon path is skipped.
			if tc.wantRelabel {
				_ = fs.WriteFile("/mnt/root/etc/selinux/config",
					[]byte("SELINUX=enforcing\nSELINUXTYPE=targeted\n"), 0o644)
			} else if tc.selinuxDisabled {
				_ = fs.WriteFile("/mnt/root/etc/selinux/config",
					[]byte("SELINUX=disabled\nSELINUXTYPE=targeted\n"), 0o644)
			}
			exec := newMockExec()
			deps := Deps{Exec: exec, FS: fs, WorkDir: "/tmp/test"}
			spec := jobs.InstallSpec{
				JobID: "job-selinux",
				Profile: profiles.Profile{OSFamily: "rhel7",
					HostnameTemplate: "host-{uuid8}",
					RootPasswordHash: "$6$h$" + strings.Repeat("c", 86),
					Network: profiles.NetworkConfig{
						Method:      "dhcp",
						NICSelector: "auto",
					},
				},
				Binding: bindings.Binding{
					MachineUUID: "deadbeef000000000000000000000000",
				},
			}
			if err := BuildSeed(context.Background(), deps, spec, "/mnt/root"); err != nil {
				t.Fatalf("BuildSeed err: %v", err)
			}
			// RHEL family should run restorecon in chroot.
			foundRestorecon := false
			for _, c := range exec.Calls() {
				if c.Name == "chroot" && len(c.Args) >= 2 && c.Args[1] == "restorecon" {
					foundRestorecon = true
					break
				}
			}
			if foundRestorecon != tc.wantRelabel {
				t.Fatalf("restorecon called=%v, want %v", foundRestorecon, tc.wantRelabel)
			}
			// /.autorelabel should NOT be present when restorecon succeeds (mock returns success).
			if tc.wantRelabel {
				if _, ok := fs.files["/mnt/root/.autorelabel"]; ok {
					t.Fatal("/.autorelabel should not be present when restorecon succeeds")
				}
			}
		})
	}
}

// ---- writeIfcfg -------------------------------------------------------------

func TestWriteIfcfg_StaticByMAC(t *testing.T) {
	fs := newMockFS()
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "192.168.10.1",
		DNS:         []string{"223.5.5.5", "8.8.8.8"},
		NICSelector: "by-mac:24:6e:96:4f:f8:f0",
	}
	b := bindings.Binding{StaticAddress: "192.168.10.190"}
	nics := []NICInfo{
		{Name: "eno1", MAC: "24:6e:96:4f:f8:f0"},
		{Name: "eno2", MAC: "24:6e:96:4f:f8:f1"},
	}
	writeIfcfg(Deps{FS: fs}, "/mnt/root", nc, b, nics)

	// MAC at index 0 → ifcfg-eth0.
	path := "/mnt/root/etc/sysconfig/network-scripts/ifcfg-eth0"
	data, ok := fs.files[path]
	if !ok {
		t.Fatalf("ifcfg-eth0 not written at %s", path)
	}
	content := string(data)
	wants := []string{
		"DEVICE=eth0",
		"NAME=eth0",
		// HWADDR is upper-case per initscripts convention.
		"HWADDR=24:6E:96:4F:F8:F0",
		"ONBOOT=yes",
		"TYPE=Ethernet",
		"BOOTPROTO=none",
		"IPADDR=192.168.10.190",
		"PREFIX=24",
		"GATEWAY=192.168.10.1",
		"DNS1=223.5.5.5",
		"DNS2=8.8.8.8",
	}
	for _, w := range wants {
		if !strings.Contains(content, w) {
			t.Errorf("ifcfg missing %q\n%s", w, content)
		}
	}
}

// When the operator picks a non-first NIC, the ifcfg file must target
// the right eth index — otherwise CentOS 7 applies the IP to the wrong NIC.
// Live NIC list is in ifindex order (matches CentOS 7's net.ifnames=0
// eth0..ethN ordering), so the picked MAC's slice position == eth index.
func TestWriteIfcfg_StaticByMAC_PicksRightEthIndex(t *testing.T) {
	fs := newMockFS()
	nc := profiles.NetworkConfig{
		Method:    "static",
		PrefixLen: 24,
		Gateway:   "192.168.10.1",
		// Picks the SECOND NIC.
		NICSelector: "by-mac:24:6e:96:4f:f8:f1",
	}
	b := bindings.Binding{StaticAddress: "192.168.10.192"}
	nics := []NICInfo{
		{Name: "eno1", MAC: "24:6e:96:4f:f8:f0"},
		{Name: "eno2", MAC: "24:6e:96:4f:f8:f1"},
		{Name: "eno3", MAC: "24:6e:96:4f:f8:f2"},
	}
	writeIfcfg(Deps{FS: fs}, "/mnt/root", nc, b, nics)

	// Should be eth1, NOT eth0.
	if _, ok := fs.files["/mnt/root/etc/sysconfig/network-scripts/ifcfg-eth1"]; !ok {
		var have []string
		for k := range fs.files {
			have = append(have, k)
		}
		t.Fatalf("expected ifcfg-eth1; got files: %v", have)
	}
	if _, ok := fs.files["/mnt/root/etc/sysconfig/network-scripts/ifcfg-eth0"]; ok {
		t.Fatalf("unexpected ifcfg-eth0 — second NIC must land on eth1")
	}
	content := string(fs.files["/mnt/root/etc/sysconfig/network-scripts/ifcfg-eth1"])
	for _, w := range []string{"DEVICE=eth1", "NAME=eth1", "HWADDR=24:6E:96:4F:F8:F1", "IPADDR=192.168.10.192"} {
		if !strings.Contains(content, w) {
			t.Errorf("ifcfg-eth1 missing %q\n%s", w, content)
		}
	}
}

func TestWriteIfcfg_DHCP(t *testing.T) {
	fs := newMockFS()
	nc := profiles.NetworkConfig{
		Method:      "dhcp",
		NICSelector: "auto",
	}
	writeIfcfg(Deps{FS: fs}, "/mnt/root", nc, bindings.Binding{}, nil)

	path := "/mnt/root/etc/sysconfig/network-scripts/ifcfg-eth0"
	data, ok := fs.files[path]
	if !ok {
		t.Fatalf("ifcfg-eth0 not written at %s", path)
	}
	content := string(data)
	if !strings.Contains(content, "BOOTPROTO=dhcp") {
		t.Fatalf("DHCP ifcfg missing BOOTPROTO=dhcp:\n%s", content)
	}
	if strings.Contains(content, "HWADDR") {
		t.Log("no HWADDR in auto-mode DHCP — expected, device matching is best-effort")
	}
}

func TestWriteIfcfg_ByName_NoHWADDR(t *testing.T) {
	fs := newMockFS()
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "10.0.0.1",
		DNS:         []string{"10.0.0.53"},
		NICSelector: "by-name:eno1",
	}
	b := bindings.Binding{StaticAddress: "10.0.0.100"}
	writeIfcfg(Deps{FS: fs}, "/mnt/root", nc, b, nil)

	// by-name with a real ifname (not MAC) → no HWADDR pinning, falls back to eth0.
	content := string(fs.files["/mnt/root/etc/sysconfig/network-scripts/ifcfg-eth0"])
	if strings.Contains(content, "HWADDR") {
		t.Fatal("by-name should not emit HWADDR")
	}
	if !strings.Contains(content, "IPADDR=10.0.0.100") {
		t.Fatal("missing IPADDR")
	}
}

// ---- BuildSeed on RHEL 7 ---------------------------------------------------

func TestBuildSeed_RHEL7_WritesBothIfcfgAndNetworkConfig(t *testing.T) {
	fs := newMockFS()
	// Simulate a CentOS 7 rootfs: os-release at /etc/os-release.
	if err := fs.MkdirAll("/mnt/root/etc", 0o755); err != nil {
		t.Fatal(err)
	}
	osRelease := `NAME="CentOS Linux"
VERSION="7 (Core)"
ID="centos"
ID_LIKE="rhel fedora"
VERSION_ID="7"`
	if err := fs.WriteFile("/mnt/root/etc/os-release", []byte(osRelease), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := Deps{Exec: newMockExec(), FS: fs, WorkDir: "/tmp/test-install"}
	spec := jobs.InstallSpec{
		JobID: "job-centos7",
		Profile: profiles.Profile{OSFamily: "rhel7",
			HostnameTemplate: "centos7-{uuid8}",
			RootPasswordHash: "$6$h$" + strings.Repeat("b", 86),
			Network: profiles.NetworkConfig{
				Method:      "static",
				PrefixLen:   24,
				Gateway:     "192.168.10.1",
				DNS:         []string{"223.5.5.5"},
				NICSelector: "by-mac:24:6e:96:4f:f8:f0",
			},
		},
		Binding: bindings.Binding{
			MachineUUID:   "deadbeef000000000000000000000000",
			StaticAddress: "192.168.10.190",
		},
	}

	if err := BuildSeed(context.Background(), deps, spec, "/mnt/root"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// ifcfg must be written.
	ifcfgPath := "/mnt/root/etc/sysconfig/network-scripts/ifcfg-eth0"
	if _, ok := fs.files[ifcfgPath]; !ok {
		t.Fatalf("ifcfg-eth0 not written at %s", ifcfgPath)
	}

	// network-config must ALSO be written for RHEL 7 so cloud-init
	// has a v2 config to process (prevents fallback to DHCP).
	netCfgPath := "/mnt/root/var/lib/cloud/seed/nocloud-net/network-config"
	if _, ok := fs.files[netCfgPath]; !ok {
		t.Fatalf("network-config should be written for RHEL 7 (missing at %s)", netCfgPath)
	}

	// user-data and meta-data must still be present.
	seedDir := "/mnt/root/var/lib/cloud/seed/nocloud-net"
	for _, name := range []string{"user-data", "meta-data"} {
		if _, ok := fs.files[seedDir+"/"+name]; !ok {
			t.Fatalf("%s not written under %s", name, seedDir)
		}
	}
}

func TestBuildSeed_Rocky9_StillWritesNetworkConfig(t *testing.T) {
	fs := newMockFS()
	if err := fs.MkdirAll("/mnt/root/etc", 0o755); err != nil {
		t.Fatal(err)
	}
	osRelease := `NAME="Rocky Linux"
VERSION="9.3 (Blue Onyx)"
ID="rocky"
ID_LIKE="rhel fedora"
VERSION_ID="9.3"`
	if err := fs.WriteFile("/mnt/root/etc/os-release", []byte(osRelease), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := Deps{Exec: newMockExec(), FS: fs, WorkDir: "/tmp/test-install"}
	spec := jobs.InstallSpec{
		JobID: "job-rocky9",
		Profile: profiles.Profile{OSFamily: "rhel",
			HostnameTemplate: "rocky-{uuid8}",
			RootPasswordHash: "$6$h$" + strings.Repeat("c", 86),
			Network:          profiles.NetworkConfig{Method: "dhcp", NICSelector: "auto"},
		},
		Binding: bindings.Binding{MachineUUID: "abcd"},
	}

	if err := BuildSeed(context.Background(), deps, spec, "/mnt/root"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// Rocky 9 (RHEL 9) must still get network-config (cloud-init supports v2).
	netCfgPath := "/mnt/root/var/lib/cloud/seed/nocloud-net/network-config"
	if _, ok := fs.files[netCfgPath]; !ok {
		t.Fatalf("network-config should be written for Rocky 9 (RHEL 9 supports v2)")
	}

	// ifcfg must NOT be written for non-RHEL7.
	ifcfgPath := "/mnt/root/etc/sysconfig/network-scripts/ifcfg-eth0"
	if _, ok := fs.files[ifcfgPath]; ok {
		t.Fatalf("ifcfg-eth0 should NOT be written for Rocky 9")
	}

	// Rocky 9 uses NetworkManager — cloud-init must be told to emit
	// `.nmconnection` keyfiles, not legacy ifcfg, otherwise `nmcli con
	// show` doesn't see the network config we just configured.
	rendererPath := "/mnt/root/etc/cloud/cloud.cfg.d/98-metalkit-renderer.cfg"
	rendererData, ok := fs.files[rendererPath]
	if !ok {
		t.Fatalf("renderer override (%s) must be written for Rocky 9", rendererPath)
	}
	if !strings.Contains(string(rendererData), "network-manager") {
		t.Fatalf("renderer override must list network-manager first; got: %s", rendererData)
	}
}

// CentOS 7 / RHEL 7 cloud-init 19.4 has no NetworkManager renderer. We
// must NOT pin the renderer there, otherwise cloud-init falls through to
// the next renderer and produces no network config at all.
func TestBuildSeed_CentOS7_DoesNotEmitNMRenderer(t *testing.T) {
	fs := newMockFS()
	if err := fs.MkdirAll("/mnt/root/etc", 0o755); err != nil {
		t.Fatal(err)
	}
	osRelease := `NAME="CentOS Linux"
VERSION="7 (Core)"
ID="centos"
ID_LIKE="rhel fedora"
VERSION_ID="7"`
	if err := fs.WriteFile("/mnt/root/etc/os-release", []byte(osRelease), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := Deps{Exec: newMockExec(), FS: fs, WorkDir: "/tmp/test-install"}
	spec := jobs.InstallSpec{
		JobID: "job-centos7",
		Profile: profiles.Profile{OSFamily: "rhel7",
			HostnameTemplate: "centos7-{uuid8}",
			RootPasswordHash: "$6$h$" + strings.Repeat("c", 86),
			Network:          profiles.NetworkConfig{Method: "dhcp", NICSelector: "auto"},
		},
		Binding: bindings.Binding{MachineUUID: "abcd"},
	}

	if err := BuildSeed(context.Background(), deps, spec, "/mnt/root"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	rendererPath := "/mnt/root/etc/cloud/cloud.cfg.d/98-metalkit-renderer.cfg"
	if _, ok := fs.files[rendererPath]; ok {
		t.Fatal("CentOS 7 must NOT get the network-manager renderer override (its cloud-init 19.4 has no NM renderer)")
	}
}

// Ubuntu uses netplan natively; no renderer override should be written.
func TestBuildSeed_Ubuntu_DoesNotEmitNMRenderer(t *testing.T) {
	fs := newMockFS()
	if err := fs.MkdirAll("/mnt/root/etc", 0o755); err != nil {
		t.Fatal(err)
	}
	osRelease := `NAME="Ubuntu"
VERSION="22.04.4 LTS (Jammy Jellyfish)"
ID=ubuntu
VERSION_ID="22.04"`
	if err := fs.WriteFile("/mnt/root/etc/os-release", []byte(osRelease), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := Deps{Exec: newMockExec(), FS: fs, WorkDir: "/tmp/test-install"}
	spec := jobs.InstallSpec{
		JobID: "job-ubuntu",
		Profile: profiles.Profile{OSFamily: "ubuntu",
			HostnameTemplate: "ubuntu-{uuid8}",
			RootPasswordHash: "$6$h$" + strings.Repeat("c", 86),
			Network:          profiles.NetworkConfig{Method: "dhcp", NICSelector: "auto"},
		},
		Binding: bindings.Binding{MachineUUID: "abcd"},
	}

	if err := BuildSeed(context.Background(), deps, spec, "/mnt/root"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	rendererPath := "/mnt/root/etc/cloud/cloud.cfg.d/98-metalkit-renderer.cfg"
	if _, ok := fs.files[rendererPath]; ok {
		t.Fatal("Ubuntu must NOT get the network-manager renderer override (netplan is native)")
	}
}

func TestBuildSeed_RHEL7_Bond_WritesIfcfgFallback(t *testing.T) {
	fs := newMockFS()
	if err := fs.MkdirAll("/mnt/root/etc", 0o755); err != nil {
		t.Fatal(err)
	}
	osRelease := `NAME="CentOS Linux"
VERSION="7 (Core)"
ID="centos"
ID_LIKE="rhel fedora"
VERSION_ID="7"`
	if err := fs.WriteFile("/mnt/root/etc/os-release", []byte(osRelease), 0o644); err != nil {
		t.Fatal(err)
	}

	deps := Deps{
		Exec:    newMockExec(),
		FS:      fs,
		WorkDir: "/tmp/test-install",
		NICs: []NICInfo{
			{Name: "eno1", MAC: "aa:bb:cc:dd:ee:01"},
			{Name: "eno2", MAC: "aa:bb:cc:dd:ee:02"},
		},
	}
	spec := jobs.InstallSpec{
		JobID: "job-centos7-bond",
		Profile: profiles.Profile{OSFamily: "rhel7",
			HostnameTemplate: "centos7-{uuid8}",
			RootPasswordHash: "$6$h$" + strings.Repeat("b", 86),
			Network: profiles.NetworkConfig{
				Method:      "static",
				PrefixLen:   24,
				Gateway:     "192.168.10.1",
				DNS:         []string{"223.5.5.5"},
				NICSelector: "auto",
				Bond: &profiles.BondConfig{
					Mode:   "active-backup",
					Slaves: []string{"eno1", "eno2"},
				},
			},
		},
		Binding: bindings.Binding{
			MachineUUID:   "deadbeef000000000000000000000000",
			StaticAddress: "192.168.10.191",
		},
	}

	if err := BuildSeed(context.Background(), deps, spec, "/mnt/root"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// network-config must be written (cloud-init v2 bond rendering).
	netCfgPath := "/mnt/root/var/lib/cloud/seed/nocloud-net/network-config"
	cfgData, ok := fs.files[netCfgPath]
	if !ok {
		t.Fatalf("network-config should be written for RHEL 7 bond")
	}
	cfgContent := string(cfgData)
	for _, sub := range []string{"eth0:", "eth1:", "interfaces: [eth0, eth1]"} {
		if !strings.Contains(cfgContent, sub) {
			t.Errorf("network-config bond missing %q\n%s", sub, cfgContent)
		}
	}

	// ifcfg-bond0 must be written (master with IP + BONDING_OPTS).
	bondPath := "/mnt/root/etc/sysconfig/network-scripts/ifcfg-bond0"
	bondData, ok := fs.files[bondPath]
	if !ok {
		t.Fatalf("ifcfg-bond0 should be written for RHEL 7 bond fallback")
	}
	bondContent := string(bondData)
	for _, sub := range []string{
		"DEVICE=bond0",
		"TYPE=Bond",
		"BONDING_MASTER=yes",
		"BOOTPROTO=none",
		"IPADDR=192.168.10.191",
		"PREFIX=24",
		"GATEWAY=192.168.10.1",
		"DNS1=223.5.5.5",
		"mode=active-backup",
		"miimon=100",
	} {
		if !strings.Contains(bondContent, sub) {
			t.Errorf("ifcfg-bond0 missing %q\n%s", sub, bondContent)
		}
	}

	// ifcfg-eth0 and ifcfg-eth1 must be written as slave configs.
	for i, expectMAC := range []string{"AA:BB:CC:DD:EE:01", "AA:BB:CC:DD:EE:02"} {
		dev := fmt.Sprintf("eth%d", i)
		path := "/mnt/root/etc/sysconfig/network-scripts/ifcfg-" + dev
		data, ok := fs.files[path]
		if !ok {
			t.Fatalf("ifcfg-%s should be written for RHEL 7 bond slave", dev)
		}
		content := string(data)
		for _, sub := range []string{
			"DEVICE=" + dev,
			"TYPE=Ethernet",
			"HWADDR=" + expectMAC,
			"MASTER=bond0",
			"SLAVE=yes",
			"BOOTPROTO=none",
		} {
			if !strings.Contains(content, sub) {
				t.Errorf("ifcfg-%s missing %q\n%s", dev, sub, content)
			}
		}
	}
}

// --- openEuler seed tests ---------------------------------------------------

// When cloud-init is absent from the target rootfs, BuildSeed must write
// hostname, password, sshd config, and network config directly.
func TestBuildSeed_NoCloudInit_WritesDirectConfig(t *testing.T) {
	fs := newMockFS()
	exec := newMockExec()
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="openEuler"` + "\n")
	fs.files["/mnt/root/etc/shadow"] = []byte("root:$6$oldhash:19000:0:99999:7:::\n")
	fs.files["/mnt/root/etc/hostname"] = []byte("oldhost\n")
	deps := Deps{
		Exec: exec,
		FS:   fs,
		NICs: []NICInfo{{Name: "eno1", MAC: "AA:BB:CC:DD:EE:01"}},
	}

	spec := jobs.InstallSpec{
		Profile: profiles.Profile{OSFamily: "openeuler",
			HostnameTemplate: "node-{uuid8}",
			RootPasswordHash: "$6$newhash",
			Network: profiles.NetworkConfig{
				Method:      "static",
				NICSelector: "by-name:eno1",
				PrefixLen:   24,
				Gateway:     "192.168.10.1",
				DNS:         []string{"223.5.5.5"},
			},
		},
		Binding: bindings.Binding{
			MachineUUID:   "abcdef01-0000-0000-0000-000000000000",
			StaticAddress: "192.168.10.100",
		},
	}

	if err := BuildSeed(context.Background(), deps, spec, "/mnt/root"); err != nil {
		t.Fatalf("BuildSeed failed: %v", err)
	}

	hostData, ok := fs.files["/mnt/root/etc/hostname"]
	if !ok {
		t.Fatal("direct-write: /etc/hostname not written")
	}
	if strings.TrimSpace(string(hostData)) != "node-abcdef01" {
		t.Fatalf("hostname = %q, want node-abcdef01", string(hostData))
	}

	shadowData, ok := fs.files["/mnt/root/etc/shadow"]
	if !ok {
		t.Fatal("direct-write: /etc/shadow not written")
	}
	if !strings.Contains(string(shadowData), "$6$newhash") {
		t.Fatalf("shadow missing new password hash:\n%s", string(shadowData))
	}

	sshdData, ok := fs.files["/mnt/root/etc/ssh/sshd_config.d/99-metalkit.conf"]
	if !ok {
		t.Fatal("direct-write: sshd drop-in not written")
	}
	sshdStr := string(sshdData)
	if !strings.Contains(sshdStr, "PermitRootLogin yes") || !strings.Contains(sshdStr, "PasswordAuthentication yes") {
		t.Fatalf("sshd drop-in missing expected content:\n%s", sshdStr)
	}

	nmData, ok := fs.files["/mnt/root/etc/NetworkManager/system-connections/metalkit-eno1.nmconnection"]
	if !ok {
		t.Fatal("direct-write: NM keyfile not written")
	}
	nmStr := string(nmData)
	for _, sub := range []string{
		"[connection]",
		"type=ethernet",
		"method=manual",
		"address1=192.168.10.100/24",
		"gateway=192.168.10.1",
		"dns=223.5.5.5",
	} {
		if !strings.Contains(nmStr, sub) {
			t.Errorf("NM keyfile missing %q\n%s", sub, nmStr)
		}
	}
}

// When cloud-init IS present, BuildSeed must NOT write hostname/password
// directly (cloud-init will handle them via the seed).
func TestBuildSeed_WithCloudInit_NoDirectWrite(t *testing.T) {
	fs := newMockFS()
	exec := newMockExec()
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID=ubuntu` + "\n")
	fs.files["/mnt/root/usr/bin/cloud-init"] = []byte("#!/bin/sh\n")
	fs.files["/mnt/root/etc/shadow"] = []byte("root:$6$oldhash:19000:0:99999:7:::\n")
	fs.files["/mnt/root/etc/hostname"] = []byte("oldhost\n")
	deps := Deps{Exec: exec, FS: fs}

	spec := jobs.InstallSpec{
		Profile: profiles.Profile{OSFamily: "ubuntu",
			HostnameTemplate: "node-{uuid8}",
			RootPasswordHash: "$6$newhash",
			Network:          profiles.NetworkConfig{Method: "dhcp"},
		},
		Binding: bindings.Binding{MachineUUID: "abcdef01-0000-0000-0000-000000000000"},
	}

	if err := BuildSeed(context.Background(), deps, spec, "/mnt/root"); err != nil {
		t.Fatalf("BuildSeed failed: %v", err)
	}

	hostData, _ := fs.files["/mnt/root/etc/hostname"]
	if strings.TrimSpace(string(hostData)) == "node-abcdef01" {
		t.Fatal("hostname should NOT be directly written when cloud-init is present")
	}

	shadowData, _ := fs.files["/mnt/root/etc/shadow"]
	if strings.Contains(string(shadowData), "$6$newhash") {
		t.Fatal("shadow should NOT be directly modified when cloud-init is present")
	}
}

// No cloud-init + bond: NM keyfiles for bond master + slaves must be written.
func TestBuildSeed_NoCloudInit_Bond_NMKeyfiles(t *testing.T) {
	fs := newMockFS()
	exec := newMockExec()
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="openEuler"` + "\n")
	fs.files["/mnt/root/etc/shadow"] = []byte("root:$6$old:19000:0:99999:7:::\n")
	deps := Deps{
		Exec: exec,
		FS:   fs,
		NICs: []NICInfo{
			{Name: "eno1", MAC: "AA:BB:CC:DD:EE:01"},
			{Name: "eno2", MAC: "AA:BB:CC:DD:EE:02"},
		},
	}

	spec := jobs.InstallSpec{
		Profile: profiles.Profile{OSFamily: "openeuler",
			HostnameTemplate: "node-{uuid8}",
			RootPasswordHash: "$6$hash",
			Network: profiles.NetworkConfig{
				Method:    "static",
				PrefixLen: 24,
				Gateway:   "192.168.10.1",
				DNS:       []string{"223.5.5.5"},
				Bond: &profiles.BondConfig{
					Mode:    "active-backup",
					Miimon:  100,
					Slaves:  []string{"by-name:eno1", "by-name:eno2"},
					Primary: "by-name:eno1",
				},
			},
		},
		Binding: bindings.Binding{
			MachineUUID:   "abcdef01-0000-0000-0000-000000000000",
			StaticAddress: "192.168.10.100",
		},
	}

	if err := BuildSeed(context.Background(), deps, spec, "/mnt/root"); err != nil {
		t.Fatalf("BuildSeed failed: %v", err)
	}

	bondData, ok := fs.files["/mnt/root/etc/NetworkManager/system-connections/bond0.nmconnection"]
	if !ok {
		t.Fatal("direct-write: bond0.nmconnection not written")
	}
	bondStr := string(bondData)
	for _, sub := range []string{
		"[connection]",
		"type=bond",
		"interface-name=bond0",
		"mode=active-backup",
		"miimon=100",
		"primary=eno1",
		"method=manual",
		"address1=192.168.10.100/24",
	} {
		if !strings.Contains(bondStr, sub) {
			t.Errorf("bond0.nmconnection missing %q\n%s", sub, bondStr)
		}
	}

	for i, mac := range []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"} {
		path := fmt.Sprintf("/mnt/root/etc/NetworkManager/system-connections/bond0-slave-%d.nmconnection", i)
		data, ok := fs.files[path]
		if !ok {
			t.Fatalf("direct-write: %s not written", path)
		}
		content := string(data)
		if !strings.Contains(content, "master=bond0") {
			t.Errorf("slave %d missing master=bond0:\n%s", i, content)
		}
		if !strings.Contains(content, "slave-type=bond") {
			t.Errorf("slave %d missing slave-type=bond:\n%s", i, content)
		}
		if !strings.Contains(content, "mac-address="+mac) {
			t.Errorf("slave %d missing mac-address=%s:\n%s", i, mac, content)
		}
		if strings.Contains(content, "interface-name=") {
			t.Errorf("slave %d should NOT have interface-name:\n%s", i, content)
		}
	}
}
