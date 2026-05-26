package installer

import (
	"context"
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
		Profile: profiles.Profile{
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
		"- [systemctl, restart, ssh]",
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
		Profile: profiles.Profile{
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
		"- [systemctl, restart, ssh]",
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
	got := renderNetworkConfig(nc, b, nil)

	mustContain := []string{
		"version: 2",
		"ethernets:",
		"  metalkit0:",
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

func TestRenderNetworkConfig_ByName(t *testing.T) {
	nc := profiles.NetworkConfig{
		Method:      "static",
		PrefixLen:   24,
		Gateway:     "192.168.1.1",
		NICSelector: "by-name:eno1",
	}
	b := bindings.Binding{StaticAddress: "192.168.1.10"}
	got := renderNetworkConfig(nc, b, nil)
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
	got := renderNetworkConfig(nc, b, nil)
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
	// No NICs passed — bond slaves fall back to match.name.
	got := renderNetworkConfig(nc, b, nil)

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
	// Live NICs available — bond slaves resolve to match.macaddress.
	nics := []NICInfo{
		{Name: "eno1", MAC: "aa:bb:cc:dd:ee:01"},
		{Name: "eno2", MAC: "aa:bb:cc:dd:ee:02"},
	}
	got := renderNetworkConfig(nc, b, nics)

	mustContain := []string{
		`      macaddress: "aa:bb:cc:dd:ee:01"`,
		`      macaddress: "aa:bb:cc:dd:ee:02"`,
		"      primary: eno1",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Fatalf("bond with NICs missing %q\nGOT:\n%s", sub, got)
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
	got := renderNetworkConfig(nc, bindings.Binding{}, nil)

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
	got := renderNetworkConfig(nc, bindings.Binding{}, nil)
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
	got := renderNetworkConfig(nc, b, nil)

	mustContain := []string{
		"ethernets:",
		"  metalkit0:",
		`      macaddress: "aa:bb:cc:dd:ee:ff"`,
		"vlans:",
		"  metalkit0.100:",
		"    id: 100",
		"    link: metalkit0",
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
	got := renderNetworkConfig(nc, bindings.Binding{}, nil)

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
	got := renderNetworkConfig(nc, bindings.Binding{}, nil)
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
		Profile: profiles.Profile{
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
