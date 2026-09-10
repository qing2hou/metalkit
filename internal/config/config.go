package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DHCPMode controls whether the embedded DHCP server hands out IP leases
// in addition to its PXE-boot advertisement role.
//
//   - "proxy" (default, back-compat): only responds to PXEClient requests
//     with boot-info; a separate DHCP server on the LAN must hand out IPs.
//   - "full": acts as a complete DHCP server, allocating IPs from DHCPPool
//     in addition to delivering PXE boot info. Use when no external DHCP
//     exists on the wire.
const (
	DHCPModeProxy = "proxy"
	DHCPModeFull  = "full"
)

type Config struct {
	ServerIP  string `yaml:"serverIP"`
	Interface string `yaml:"interface"`
	HTTPAddr  string `yaml:"httpAddr"`
	DHCPAddr  string `yaml:"dhcpAddr"`
	BSDPAddr  string `yaml:"bsdpAddr"`
	TFTPAddr  string `yaml:"tftpAddr"`
	BootDir   string `yaml:"bootDir"`
	LogLevel  string `yaml:"logLevel"`

	// DHCPMode toggles full DHCP vs the legacy ProxyDHCP behaviour. Defaults
	// to "proxy" so existing deployments upgrade silently; set to "full" to
	// make metalkit hand out IP leases itself (zero external-DHCP deploy).
	DHCPMode string `yaml:"dhcpMode"`

	// DHCPPool is the lease pool used when DHCPMode == "full". All fields are
	// optional — Load() auto-fills any blank field by reading the IPv4/mask
	// of cfg.Interface, so a minimal "dhcpMode: full" config Just Works.
	DHCPPool *DHCPPool `yaml:"dhcpPool"`

	// Inventory store path. Defaults to /var/lib/metalkit/inventory.db.
	DBPath string `yaml:"dbPath"`

	// Image storage directory. Holds chunked-upload working dirs under .tmp/
	// and the content-addressed final images. Defaults to
	// /var/lib/metalkit/images.
	ImagesDir string `yaml:"imagesDir"`

	// MasterKeyPath holds the AES-256 key used for field-level encryption of
	// BMC passwords (and future sensitive columns). Auto-generated on first
	// boot with mode 0600 if missing. Defaults to /var/lib/metalkit/master.key.
	MasterKeyPath string `yaml:"masterKeyPath"`

	// Basic Auth credentials for the Web UI and the read-side of the inventory
	// API. AdminPass empty disables auth (open mode) and logs a warning at
	// startup. AdminUser defaults to "admin".
	AdminUser string `yaml:"adminUser"`
	AdminPass string `yaml:"adminPass"`

	// DefaultRootPassword is the plaintext password installer baselines all
	// freshly created profiles to when the operator leaves the field blank.
	// Hashed once at controller startup; the hash is what hits SQLite. Default
	// "metalkit" — change in config.yaml per-environment.
	DefaultRootPassword string `yaml:"defaultRootPassword"`

	// HTTPS serves the HTTP port over TLS. Users holds the operator
	// accounts for the Web UI (replaces the single adminUser/adminPass pair
	// when non-empty; the legacy pair remains as fallback for back-compat).
	HTTPS *HTTPSConfig `yaml:"https"`
	Users []User       `yaml:"users"`
}

// HTTPSConfig enables TLS alongside HTTP. Two certificate modes:
//
//   - Static: certFile + keyFile point at operator-provided PEM files.
//     Both must be set together.
//   - Auto: certFile/keyFile empty → a self-signed certificate is generated
//     on first boot into stateDir (default under the data dir), with SANs for
//     ServerIP, localhost and the interface IP. Suitable for management-
//     network deployments where the controller is the TLS root of its own
//     browsing population (operators accept the browser warning or
//     distribute the cert).
//
// Dual-listener model (PXE machines cannot do TLS — no trust store, and the
// shipped iPXE binaries chain over plain HTTP):
//
//   - httpsAddr (default ":8443") speaks TLS and serves EVERYTHING.
//   - The original httpAddr keeps speaking HTTP but serves only the machine
//     channel (/boot/*, /healthz, /api/v1/report, /api/v1/heartbeat/*,
//     /api/v1/agent/*); every human-facing path (/ui, the operator API)
//     is 308-redirected to the HTTPS listener. iPXE templates and the
//     agent's metalkit.url keep pointing at the HTTP listener untouched.
type HTTPSConfig struct {
	// HTTPSAddr is the TLS listener. Defaults to ":8443".
	HTTPSAddr   string `yaml:"httpsAddr"`
	CertFile    string `yaml:"certFile"`
	KeyFile     string `yaml:"keyFile"`
	StateDir    string `yaml:"stateDir"`    // where the auto cert/key live; defaults to <dbdir>/tls
	AutoDNSName string `yaml:"autoDNSName"` // extra SAN (e.g. metalkit.internal)
}

// User is an operator account for the Web UI / API. Password is verified
// against the bcrypt-style hash produced by `mkpasswd -m sha-512` at
// account-creation time (same primitive util.CryptSHA512 uses).
type User struct {
	Username string `yaml:"username"`
	// PassHash is a $6$ sha512crypt hash. Generate with:
	//   mkpasswd -m sha-512
	PassHash string `yaml:"passHash"`
	// Role: "admin" (default) can do everything including user management;
	// "operator" can operate but not manage users or read passwords.
	Role string `yaml:"role"`
}

// DHCPPool is the lease range and per-subnet metadata for full DHCP mode.
// All fields are optional in YAML; Load() fills the blanks from the
// interface's address + mask. CIDR is implied by Netmask + ServerIP.
type DHCPPool struct {
	Start      string   `yaml:"start"`      // first leasable IP
	End        string   `yaml:"end"`        // last leasable IP
	Netmask    string   `yaml:"netmask"`    // dotted-quad mask, e.g. 255.255.255.0
	Gateway    string   `yaml:"gateway"`    // option 3 (router)
	DNS        []string `yaml:"dns"`        // option 6 (DNS servers)
	LeaseHours int      `yaml:"leaseHours"` // option 51 (lease time)
	Exclude    []string `yaml:"exclude"`    // IPs we must never hand out
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if c.Interface == "" {
		return nil, fmt.Errorf("interface: required")
	}

	// Auto-detect serverIP from interface if not specified
	if c.ServerIP == "" {
		detectedIP, err := detectInterfaceIP(c.Interface)
		if err != nil {
			return nil, fmt.Errorf("auto-detect serverIP from %s: %w", c.Interface, err)
		}
		c.ServerIP = detectedIP
		slog.Info("auto-detected serverIP", "interface", c.Interface, "ip", c.ServerIP)
	}

	addr, err := netip.ParseAddr(c.ServerIP)
	if err != nil {
		return nil, fmt.Errorf("serverIP %q: %w", c.ServerIP, err)
	}
	if !addr.Is4() {
		return nil, fmt.Errorf("serverIP %q: must be IPv4", c.ServerIP)
	}
	if c.BSDPAddr == "" {
		c.BSDPAddr = ":4011"
	}
	if c.DBPath == "" {
		c.DBPath = "/var/lib/metalkit/inventory.db"
	}
	if c.ImagesDir == "" {
		c.ImagesDir = "/var/lib/metalkit/images"
	}
	if c.MasterKeyPath == "" {
		c.MasterKeyPath = "/var/lib/metalkit/master.key"
	}
	if c.AdminUser == "" {
		c.AdminUser = "admin"
	}
	if c.DefaultRootPassword == "" {
		c.DefaultRootPassword = "metalkit"
	}

	// HTTPS cert pairs must be complete when the static mode is used.
	if c.HTTPS != nil {
		h := c.HTTPS
		if (h.CertFile == "") != (h.KeyFile == "") {
			return nil, fmt.Errorf("https: certFile and keyFile must be set together")
		}
		if h.HTTPSAddr == "" {
			h.HTTPSAddr = ":8443"
		}
		if h.StateDir == "" {
			h.StateDir = filepath.Join(filepath.Dir(c.DBPath), "tls")
		}
	}

	// User roles normalize to a closed set; empty Role means admin for
	// back-compat with hand-written configs.
	for i := range c.Users {
		switch r := strings.ToLower(strings.TrimSpace(c.Users[i].Role)); r {
		case "":
			c.Users[i].Role = "admin"
		case "admin", "operator":
			c.Users[i].Role = r
		default:
			return nil, fmt.Errorf("users[%d]: role %q must be admin or operator", i, c.Users[i].Role)
		}
	}

	// Normalize DHCP mode. Unset and unknown values fall back to proxy so
	// typos never silently flip a live deployment to full mode.
	switch strings.ToLower(strings.TrimSpace(c.DHCPMode)) {
	case "", DHCPModeProxy:
		c.DHCPMode = DHCPModeProxy
	case DHCPModeFull:
		c.DHCPMode = DHCPModeFull
	default:
		return nil, fmt.Errorf("dhcpMode %q: must be %q or %q", c.DHCPMode, DHCPModeProxy, DHCPModeFull)
	}

	if c.DHCPMode == DHCPModeFull {
		if err := c.ResolveDHCPPool(); err != nil {
			return nil, fmt.Errorf("dhcpPool: %w", err)
		}
	}
	return &c, nil
}

// ResolveDHCPPool fills in any blank DHCPPool fields by reading the IPv4
// address and mask of cfg.Interface. The goal is "zero-config full mode":
// dropping `dhcpMode: full` into config.yaml should produce a working
// pool with no further input.
//
// Exported so cmd/controller can re-run derivation after the settings
// store overlays runtime overrides (e.g. UI flipped mode to full but no
// pool fields were ever written).
//
// Defaults derived from the interface (e.g. 192.168.10.120/24 on ens32):
//   - Netmask: from the interface's netmask         → 255.255.255.0
//   - Gateway: subnet base + 1                      → 192.168.10.1
//   - Start  : subnet base + 100                    → 192.168.10.100
//   - End    : subnet base + 200 (clamped to .254)  → 192.168.10.200
//   - DNS    : [8.8.8.8, 1.1.1.1]
//   - Exclude: [ServerIP, Gateway] auto-added
//   - LeaseHours: 24
func (c *Config) ResolveDHCPPool() error {
	if c.DHCPPool == nil {
		c.DHCPPool = &DHCPPool{}
	}
	p := c.DHCPPool

	// Discover the interface's IPv4 prefix so we can derive defaults.
	prefix, err := interfaceIPv4Prefix(c.Interface, c.ServerIP)
	if err != nil {
		return err
	}
	subnet := prefix.Masked() // network base address

	if p.Netmask == "" {
		mask := net.CIDRMask(prefix.Bits(), 32)
		p.Netmask = net.IP(mask).String()
	} else if _, err := parseIPv4Mask(p.Netmask); err != nil {
		return fmt.Errorf("netmask %q: %w", p.Netmask, err)
	}

	if p.Gateway == "" {
		gw, err := offsetIP(subnet.Addr(), 1)
		if err != nil {
			return fmt.Errorf("derive gateway: %w", err)
		}
		p.Gateway = gw.String()
	} else if _, err := netip.ParseAddr(p.Gateway); err != nil {
		return fmt.Errorf("gateway %q: %w", p.Gateway, err)
	}

	if p.Start == "" {
		start, err := offsetIP(subnet.Addr(), 100)
		if err != nil {
			return fmt.Errorf("derive start: %w", err)
		}
		p.Start = start.String()
	}
	if p.End == "" {
		end, err := offsetIP(subnet.Addr(), 200)
		if err != nil {
			return fmt.Errorf("derive end: %w", err)
		}
		p.End = end.String()
	}

	startA, err := netip.ParseAddr(p.Start)
	if err != nil || !startA.Is4() {
		return fmt.Errorf("start %q: must be IPv4", p.Start)
	}
	endA, err := netip.ParseAddr(p.End)
	if err != nil || !endA.Is4() {
		return fmt.Errorf("end %q: must be IPv4", p.End)
	}
	if startA.Compare(endA) > 0 {
		return fmt.Errorf("start %s > end %s", p.Start, p.End)
	}
	if !prefix.Contains(startA) || !prefix.Contains(endA) {
		return fmt.Errorf("pool %s–%s not inside interface subnet %s", p.Start, p.End, prefix)
	}

	if len(p.DNS) == 0 {
		p.DNS = []string{"8.8.8.8", "1.1.1.1"}
	} else {
		for _, d := range p.DNS {
			a, err := netip.ParseAddr(d)
			if err != nil || !a.Is4() {
				return fmt.Errorf("dns %q: must be IPv4", d)
			}
		}
	}

	if p.LeaseHours <= 0 {
		p.LeaseHours = 24
	}

	// Always exclude the controller IP and the configured gateway, even if
	// the operator didn't list them. Use a small set so duplicates collapse.
	excludeSet := map[string]struct{}{c.ServerIP: {}, p.Gateway: {}}
	for _, e := range p.Exclude {
		a, err := netip.ParseAddr(e)
		if err != nil || !a.Is4() {
			return fmt.Errorf("exclude %q: must be IPv4", e)
		}
		excludeSet[a.String()] = struct{}{}
	}
	merged := make([]string, 0, len(excludeSet))
	for ip := range excludeSet {
		merged = append(merged, ip)
	}
	p.Exclude = merged
	return nil
}

// detectInterfaceIP returns the first non-loopback IPv4 address on the
// specified interface. Used when serverIP is not specified in config.
func detectInterfaceIP(ifaceName string) (string, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return "", fmt.Errorf("interface %q: %w", ifaceName, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("interface %q addrs: %w", ifaceName, err)
	}
	// Find the first IPv4 address that's not a /32 (alias) or /31 (point-to-point)
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ipnet.IP.To4() == nil {
			continue
		}
		ones, _ := ipnet.Mask.Size()
		if ones >= 31 {
			continue // skip /32 aliases and /31 point-to-point
		}
		return ipnet.IP.String(), nil
	}
	return "", fmt.Errorf("no suitable IPv4 address found on %s", ifaceName)
}

// interfaceIPv4Prefix returns the IPv4 prefix on iface that contains
// serverIP — this is the prefix we'll allocate leases from. We match by
// serverIP rather than "first IPv4" because production hosts often have
// secondary addresses (e.g. .120 + a /32 .11 alias) and the pool must be
// derived from the primary network.
func interfaceIPv4Prefix(ifaceName, serverIP string) (netip.Prefix, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("interface %q: %w", ifaceName, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("interface %q addrs: %w", ifaceName, err)
	}
	srvA, err := netip.ParseAddr(serverIP)
	if err != nil {
		return netip.Prefix{}, err
	}
	// Prefer the prefix that both (a) is IPv4 and (b) contains serverIP and
	// (c) is wider than /31 (a /32 alias is not a real subnet).
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ipnet.IP.To4() == nil {
			continue
		}
		ones, _ := ipnet.Mask.Size()
		if ones >= 31 {
			continue
		}
		pfx, err := netip.ParsePrefix(ipnet.String())
		if err != nil {
			continue
		}
		if pfx.Contains(srvA) {
			return pfx, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("no IPv4 subnet on %s contains serverIP %s", ifaceName, serverIP)
}

// offsetIP adds n to the host portion of a base address (which is expected
// to already be a network base / .0). Returns an error if n overflows the
// 32-bit space, but does NOT verify n stays inside any particular prefix —
// callers do that separately so the function is reusable.
func offsetIP(base netip.Addr, n uint32) (netip.Addr, error) {
	if !base.Is4() {
		return netip.Addr{}, errors.New("base is not IPv4")
	}
	b := base.As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += n
	out := [4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	return netip.AddrFrom4(out), nil
}

// parseIPv4Mask validates that mask is a contiguous IPv4 mask in dotted form.
func parseIPv4Mask(mask string) (net.IPMask, error) {
	ip := net.ParseIP(mask)
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("not an IPv4 address")
	}
	m := net.IPMask(ip.To4())
	if _, bits := m.Size(); bits != 32 {
		return nil, fmt.Errorf("not a contiguous mask")
	}
	return m, nil
}

func (c *Config) SlogLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
