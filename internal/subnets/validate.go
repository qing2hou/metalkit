package subnets

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

const (
	MaxNameLen        = 64
	MaxDescriptionLen = 1024
)

var (
	subnetIDRE   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	subnetNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// validateCIDR parses an IPv4 CIDR like "192.168.10.0/24" and returns the
// canonical network form (host bits zeroed) along with the parsed prefix.
// Only IPv4 is accepted for M2.
func validateCIDR(s string) (string, netip.Prefix, error) {
	s = strings.TrimSpace(s)
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return "", netip.Prefix{}, fmt.Errorf("cidr %q: %w", s, err)
	}
	if !p.Addr().Is4() {
		return "", netip.Prefix{}, fmt.Errorf("cidr %q: IPv4 only", s)
	}
	if p.Bits() < 1 || p.Bits() > 32 {
		return "", netip.Prefix{}, fmt.Errorf("cidr %q: prefix length must be 1..32", s)
	}
	// Canonicalise: mask host bits so "192.168.10.50/24" → "192.168.10.0/24".
	canon := p.Masked()
	return canon.String(), canon, nil
}

// validateGateway parses a bare IPv4 address and verifies it falls inside the
// given prefix and isn't the network or broadcast address.
func validateGateway(s string, prefix netip.Prefix) (string, error) {
	s = strings.TrimSpace(s)
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return "", fmt.Errorf("gateway %q: %w", s, err)
	}
	if !addr.Is4() {
		return "", fmt.Errorf("gateway %q: IPv4 only", s)
	}
	if !prefix.Contains(addr) {
		return "", fmt.Errorf("gateway %q: not inside cidr %s", s, prefix)
	}
	if addr == prefix.Addr() {
		return "", fmt.Errorf("gateway %q: is the network address", s)
	}
	if b := broadcastOf(prefix); b.IsValid() && addr == b {
		return "", fmt.Errorf("gateway %q: is the broadcast address", s)
	}
	return addr.String(), nil
}

// validateDNS parses zero or more IPv4 addresses, deduped + canonicalised.
func validateDNS(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("dns[%d] %q: %w", i, s, err)
		}
		if !addr.Is4() {
			return nil, fmt.Errorf("dns[%d] %q: IPv4 only", i, s)
		}
		canon := addr.String()
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		out = append(out, canon)
	}
	return out, nil
}

// validateVLAN allows 0 (untagged) or 1..4094.
func validateVLAN(v int) error {
	if v == 0 {
		return nil
	}
	if v < 1 || v > 4094 {
		return fmt.Errorf("vlan_id %d: must be 0 (untagged) or 1..4094", v)
	}
	return nil
}

// HostInSubnet reports whether `ip` (IPv4 literal) is a valid host address
// inside `cidr` — i.e. inside the prefix, not the network or broadcast, and
// not equal to the gateway. Used by bindings to validate host_ip; exported so
// the binding layer can reuse the same rule.
func HostInSubnet(ip, cidr, gateway string) error {
	_, prefix, err := validateCIDR(cidr)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return fmt.Errorf("host ip %q: %w", ip, err)
	}
	if !addr.Is4() {
		return fmt.Errorf("host ip %q: IPv4 only", ip)
	}
	if !prefix.Contains(addr) {
		return fmt.Errorf("host ip %q: not inside cidr %s", ip, prefix)
	}
	if addr == prefix.Addr() {
		return fmt.Errorf("host ip %q: is the network address", ip)
	}
	if b := broadcastOf(prefix); b.IsValid() && addr == b {
		return fmt.Errorf("host ip %q: is the broadcast address", ip)
	}
	if gateway != "" {
		gw, err := netip.ParseAddr(strings.TrimSpace(gateway))
		if err == nil && addr == gw {
			return fmt.Errorf("host ip %q: equals gateway", ip)
		}
	}
	return nil
}

// broadcastOf returns the broadcast address of an IPv4 prefix, or an invalid
// Addr for /31 and /32 prefixes (where the concept doesn't apply).
func broadcastOf(p netip.Prefix) netip.Addr {
	if !p.Addr().Is4() {
		return netip.Addr{}
	}
	bits := p.Bits()
	if bits >= 31 {
		return netip.Addr{}
	}
	addrBytes := p.Addr().As4()
	hostBits := 32 - bits
	// Set the host bits to 1.
	for i := 0; i < hostBits; i++ {
		byteIdx := 3 - i/8
		bitIdx := i % 8
		addrBytes[byteIdx] |= 1 << bitIdx
	}
	return netip.AddrFrom4(addrBytes)
}
