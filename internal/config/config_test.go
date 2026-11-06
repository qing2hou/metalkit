package config

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes YAML to a temp file and returns its path.
func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// hostIPv4Prefix finds an IPv4 /30-or-wider address on a real interface so
// pool-resolution tests can run against the machine's actual network stack
// (loopback "lo" always has 127.0.0.1/8 on Linux). Returns the interface
// name and a server IP inside its prefix.
func hostIPv4(t *testing.T) (iface, serverIP string) {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("net.Interfaces: %v", err)
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			ones, bits := ipnet.Mask.Size()
			if bits != 32 || ones >= 31 {
				continue
			}
			return ifc.Name, ipnet.IP.String()
		}
	}
	t.Skip("no IPv4 interface with a /30-or-wider address")
	return "", ""
}

func TestLoadRejectsMissingInterface(t *testing.T) {
	if _, err := Load(writeConfig(t, "serverIP: 10.0.0.1\n")); err == nil {
		t.Fatal("config without interface must be rejected")
	} else if !strings.Contains(err.Error(), "interface") {
		t.Errorf("error should mention interface, got: %v", err)
	}
}

func TestLoadRejectsBadYAML(t *testing.T) {
	if _, err := Load(writeConfig(t, ":\n  - not yaml [")); err == nil {
		t.Fatal("malformed YAML must be rejected")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/metalkit-test.yaml"); err == nil {
		t.Fatal("missing file must be rejected")
	}
}

func TestLoadRejectsIPv6ServerIP(t *testing.T) {
	_, err := Load(writeConfig(t, "interface: lo\nserverIP: \"::1\"\n"))
	if err == nil || !strings.Contains(err.Error(), "must be IPv4") {
		t.Fatalf("IPv6 serverIP must be rejected: %v", err)
	}
}

// Defaults: with a pinned serverIP every optional field gets its documented
// default and ServerIPPinned records the provenance.
func TestLoadDefaultsAndPinning(t *testing.T) {
	iface, ip := hostIPv4(t)
	c, err := Load(writeConfig(t, "interface: "+iface+"\nserverIP: "+ip+"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.ServerIPPinned {
		t.Error("explicit serverIP must set ServerIPPinned")
	}
	for field, want := range map[string]string{
		"BSDPAddr":            ":4011",
		"DBPath":              "/var/lib/metalkit/inventory.db",
		"ImagesDir":           "/var/lib/metalkit/images",
		"MasterKeyPath":       "/var/lib/metalkit/master.key",
		"AdminUser":           "admin",
		"DefaultRootPassword": "metalkit",
	} {
		if got := fieldOf(c, field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	if c.DHCPMode != DHCPModeProxy {
		t.Errorf("DHCPMode default = %q, want proxy", c.DHCPMode)
	}
}

// Auto-detect path: serverIP blank → derived from interface, ServerIPPinned
// stays false. The reverse (detectInterfaceIP) is exercised implicitly here.
func TestLoadAutoDetectsServerIP(t *testing.T) {
	iface, wantIP := hostIPv4(t)
	c, err := Load(writeConfig(t, "interface: "+iface+"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ServerIP != wantIP {
		t.Errorf("auto-detected serverIP = %q, want %q", c.ServerIP, wantIP)
	}
	if c.ServerIPPinned {
		t.Error("auto-detected serverIP must not be pinned")
	}
}

func TestLoadUnknownInterfaceFails(t *testing.T) {
	if _, err := Load(writeConfig(t, "interface: metalkit-nonexistent0\n")); err == nil {
		t.Fatal("unknown interface with no serverIP must fail auto-detection")
	}
}

func TestLoadDHCPModeNormalization(t *testing.T) {
	cases := map[string]string{
		"":       DHCPModeProxy,
		"proxy":  DHCPModeProxy,
		" full ": DHCPModeFull,
		"PROXY":  DHCPModeProxy,
		"bogus":  "",
	}
	for in, want := range cases {
		iface, ip := hostIPv4(t)
		yaml := "interface: " + iface + "\nserverIP: " + ip + "\ndhcpMode: \"" + in + "\"\n"
		c, err := Load(writeConfig(t, yaml))
		if want == "" {
			if err == nil {
				t.Errorf("dhcpMode %q must be rejected", in)
			}
			continue
		}
		if err != nil {
			t.Errorf("dhcpMode %q: %v", in, err)
			continue
		}
		if c.DHCPMode != want {
			t.Errorf("dhcpMode %q normalized to %q, want %q", in, c.DHCPMode, want)
		}
	}
}

func TestLoadHTTPSValidation(t *testing.T) {
	iface, ip := hostIPv4(t)
	base := "interface: " + iface + "\nserverIP: " + ip + "\n"
	// cert without key must be rejected
	if _, err := Load(writeConfig(t, base+"https:\n  certFile: /tmp/a.pem\n")); err == nil {
		t.Error("half cert pair must be rejected")
	}
	// defaults kick in for a bare https block
	c, err := Load(writeConfig(t, base+"https:\n  autoDNSName: metalkit.internal\n"))
	if err != nil {
		t.Fatalf("bare https block: %v", err)
	}
	if c.HTTPS.HTTPSAddr != ":8443" {
		t.Errorf("https addr default = %q, want :8443", c.HTTPS.HTTPSAddr)
	}
	if c.HTTPS.CertFile != "" || c.HTTPS.KeyFile != "" {
		t.Error("auto mode must have empty cert/key files")
	}
}

func TestLoadUserRoles(t *testing.T) {
	iface, ip := hostIPv4(t)
	yaml := "interface: " + iface + "\nserverIP: " + ip + `
users:
  - username: alice
  - username: bob
    role: Operator
  - username: mallory
    role: superuser
`
	_, err := Load(writeConfig(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "superuser") {
		t.Fatalf("unknown role must be rejected: %v", err)
	}
	yaml = strings.Replace(yaml, "superuser", "operator", 1)
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load users: %v", err)
	}
	if c.Users[0].Role != "admin" {
		t.Errorf("empty role should default to admin, got %q", c.Users[0].Role)
	}
	if c.Users[1].Role != "operator" {
		t.Errorf("Operator should normalize to lower-case, got %q", c.Users[1].Role)
	}
}

// Full-mode pool derivation: a zero-field pool derives every default from
// the interface prefix; garbage fields are rejected.
func TestLoadFullModePoolDerivation(t *testing.T) {
	iface, ip := hostIPv4(t)
	pfx := prefixContaining(t, iface, ip)
	base := pfx.Masked().Addr()

	c, err := Load(writeConfig(t, "interface: "+iface+"\nserverIP: "+ip+"\ndhcpMode: full\n"))
	if err != nil {
		t.Fatalf("zero-config full mode: %v", err)
	}
	p := c.DHCPPool
	if p == nil {
		t.Fatal("dhcpMode: full must populate DHCPPool")
	}
	if got, err := offsetIP(base, 1); err != nil || p.Gateway != got.String() {
		t.Errorf("gateway = %q, want %s", p.Gateway, got)
	}
	if got, err := offsetIP(base, 100); err != nil || p.Start != got.String() {
		t.Errorf("start = %q, want %s", p.Start, got)
	}
	if got, err := offsetIP(base, 200); err != nil || p.End != got.String() {
		t.Errorf("end = %q, want %s", p.End, got)
	}
	if p.LeaseHours != 24 {
		t.Errorf("leaseHours = %d, want 24", p.LeaseHours)
	}
	// serverIP and gateway always land in Exclude.
	ex := map[string]bool{}
	for _, e := range p.Exclude {
		ex[e] = true
	}
	if !ex[ip] || !ex[p.Gateway] {
		t.Errorf("exclude must contain serverIP %s and gateway %s, got %v", ip, p.Gateway, p.Exclude)
	}
}

func TestLoadFullModeBadPoolRejected(t *testing.T) {
	iface, ip := hostIPv4(t)
	cases := []string{
		"netmask: 300.1.1.1\n",                   // not an IP
		"gateway: notanip\n",                     // garbage gateway
		"start: 10.99.99.99\nend: 10.99.99.98\n", // start > end
		"dns: [\"notanip\"]\n",                   // garbage DNS
		"exclude: [\"10.0.0.999\"]\n",            // garbage exclude
	}
	for i, extra := range cases {
		yaml := "interface: " + iface + "\nserverIP: " + ip + "\ndhcpMode: full\ndhcpPool:\n  " +
			strings.ReplaceAll(extra, "\n", "\n  ")
		yaml = strings.TrimRight(yaml, " \n") + "\n"
		if _, err := Load(writeConfig(t, yaml)); err == nil {
			t.Errorf("case %d: pool with %q must be rejected", i, strings.TrimSpace(extra))
		}
	}
}

func TestSlogLevel(t *testing.T) {
	cases := map[string]string{
		"":         "INFO",
		"debug":    "DEBUG",
		"INFO":     "INFO",
		"warn":     "WARN",
		"warning":  "WARN",
		"error":    "ERROR",
		"nonsense": "INFO",
	}
	for in, want := range cases {
		c := &Config{LogLevel: in}
		if got := c.SlogLevel().String(); got != want {
			t.Errorf("SlogLevel(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestParseIPv4Mask(t *testing.T) {
	for _, m := range []string{"255.255.255.0", "255.255.0.0", "0.0.0.0", "255.255.255.255"} {
		if _, err := parseIPv4Mask(m); err != nil {
			t.Errorf("parseIPv4Mask(%q): unexpected error %v", m, err)
		}
	}
	for _, m := range []string{"255.0.255.0", "notanip", "fe80::1"} {
		// 255.0.255.0 parses as an IP but is not a contiguous mask —
		// net.IPMask.Size reports bits==0 so parseIPv4Mask must reject it.
		if _, err := parseIPv4Mask(m); err == nil {
			t.Errorf("parseIPv4Mask(%q): must be rejected", m)
		}
	}
}

func TestOffsetIP(t *testing.T) {
	base := netip.MustParseAddr("10.0.0.0")
	got, err := offsetIP(base, 200)
	if err != nil {
		t.Fatalf("offsetIP: %v", err)
	}
	if got.String() != "10.0.0.200" {
		t.Errorf("offsetIP = %s, want 10.0.0.200", got)
	}
	if _, err := offsetIP(netip.MustParseAddr("::1"), 1); err == nil {
		t.Error("IPv6 base must be rejected")
	}
}

func fieldOf(c *Config, field string) string {
	switch field {
	case "BSDPAddr":
		return c.BSDPAddr
	case "DBPath":
		return c.DBPath
	case "ImagesDir":
		return c.ImagesDir
	case "MasterKeyPath":
		return c.MasterKeyPath
	case "AdminUser":
		return c.AdminUser
	case "DefaultRootPassword":
		return c.DefaultRootPassword
	}
	return "?"
}

// prefixContaining mirrors interfaceIPv4Prefix's selection for the test's
// own assertions (the serverIP is picked from the same address list).
func prefixContaining(t *testing.T, ifaceName, ip string) netip.Prefix {
	t.Helper()
	p, err := interfaceIPv4Prefix(ifaceName, ip)
	if err != nil {
		t.Skipf("interfaceIPv4Prefix: %v", err)
	}
	return p
}
