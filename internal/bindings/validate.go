package bindings

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"metalkit/internal/profiles"
	"metalkit/internal/subnets"
)

// Canonical SMBIOS UUID — same form the inventory store accepts.
var smbiosUUIDRE = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// 32-hex IDs as produced by images and profiles packages.
var hex32RE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// hostnameRE is the RFC-1123 valid form for a *resolved* hostname (no
// placeholders — that's already been expanded by binding time). Length ≤253
// is enforced separately. Per-label length ≤63 is also enforced separately.
var hostnameRE = regexp.MustCompile(
	`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// validDesiredStates is the closed set accepted by desired_state.
var validDesiredStates = map[string]bool{
	"none": true, "install": true, "reinstall": true,
}

// MaxHostnameLen mirrors RFC-1035 §2.3.4.
const MaxHostnameLen = 253

func validateMachineUUID(u string) (string, error) {
	u = strings.ToLower(strings.TrimSpace(u))
	if !smbiosUUIDRE.MatchString(u) {
		return "", fmt.Errorf("machine_uuid %q: must be canonical lowercase SMBIOS UUID", u)
	}
	return u, nil
}

func validateID(field, id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !hex32RE.MatchString(id) {
		return "", fmt.Errorf("%s %q: must be 32 lowercase hex chars", field, id)
	}
	return id, nil
}

func validateDesiredState(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !validDesiredStates[s] {
		return "", fmt.Errorf("desired_state %q: must be one of none/install/reinstall", s)
	}
	return s, nil
}

// validateStaticAddress enforces IPv4 literal *iff* the profile network method
// is "static". When dhcp, the field must be empty.
func validateStaticAddress(addr, networkMethod string) (string, error) {
	addr = strings.TrimSpace(addr)
	switch networkMethod {
	case "static":
		if addr == "" {
			return "", errors.New("static_address: required when profile.network.method=static")
		}
		ip := net.ParseIP(addr)
		if ip == nil || ip.To4() == nil {
			return "", fmt.Errorf("static_address %q: must be IPv4 literal", addr)
		}
		return ip.String(), nil
	case "dhcp":
		if addr != "" {
			return "", errors.New("static_address: must be empty when profile.network.method=dhcp")
		}
		return "", nil
	default:
		return "", fmt.Errorf("validateStaticAddress: unknown network method %q (profile validator bug?)", networkMethod)
	}
}

// validateHostname enforces RFC-1123/-1035 limits on a resolved hostname.
// Empty is allowed — agent will fall back to expanding the profile's
// hostname_template.
func validateHostname(h string) (string, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", nil
	}
	if len(h) > MaxHostnameLen {
		return "", fmt.Errorf("hostname length %d exceeds %d", len(h), MaxHostnameLen)
	}
	if !hostnameRE.MatchString(h) {
		return "", fmt.Errorf("hostname %q: invalid characters", h)
	}
	for _, label := range strings.Split(h, ".") {
		if l := len(label); l == 0 || l > 63 {
			return "", fmt.Errorf("hostname label %q: must be 1..63 chars", label)
		}
	}
	return h, nil
}

// validatePassword enforces the same plaintext length range as util.CryptSHA512
// (8..128). Whitespace at the edges is *not* trimmed — Unicode passwords are
// allowed and trimming would silently change them.
func validatePassword(p string) error {
	if len(p) < 8 {
		return errors.New("root_password: must be at least 8 characters")
	}
	if len(p) > 128 {
		return errors.New("root_password: longer than 128 characters")
	}
	return nil
}

// validateVLANOverride accepts 0 (= no override, fall back to subnet.vlan_id)
// or 1..4094. Anything else is rejected. nil pointer = field not provided
// (kept unchanged on PUT). Returns the canonical int and whether the caller
// should write NULL (0 → NULL so the row stays nullable).
func validateVLANOverride(p *int) (writeNull bool, val int, err error) {
	if p == nil {
		return false, 0, errors.New("validateVLANOverride: called with nil — caller bug")
	}
	v := *p
	if v == 0 {
		return true, 0, nil
	}
	if v < 1 || v > 4094 {
		return false, 0, fmt.Errorf("vlan_override %d: must be 0 (no override) or 1..4094", v)
	}
	return false, v, nil
}

// validateSubnetID accepts an empty string (= clear / DHCP, no subnet) or a
// 32 lowercase hex id. Empty maps to NULL in the row.
func validateSubnetID(id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return "", nil
	}
	if !hex32RE.MatchString(id) {
		return "", fmt.Errorf("subnet_id %q: must be 32 lowercase hex chars (or empty)", id)
	}
	return id, nil
}

// validateNICSelectorOverride accepts the same shapes as
// profile.network.nic_selector ("auto", "by-mac:<MAC>", "by-name:<ifname>"),
// plus the empty string which maps to NULL (= inherit profile). Trimmed +
// lowercased before validation so the stored form is canonical.
func validateNICSelectorOverride(sel string) (string, error) {
	sel = strings.ToLower(strings.TrimSpace(sel))
	if sel == "" {
		return "", nil
	}
	if err := profiles.ValidateNICSelector(sel); err != nil {
		return "", fmt.Errorf("nic_selector_override: %w", err)
	}
	return sel, nil
}

// hostInSubnet wraps subnets.HostInSubnet so callers in this package don't
// need to import the subnets package directly. The rule is: host must be a
// valid IPv4 inside cidr, not the network/broadcast address, and not equal
// to gateway.
func hostInSubnet(host, cidr, gateway string) error {
	return subnets.HostInSubnet(host, cidr, gateway)
}

// isClearJSON returns true when raw represents an explicit "clear" intent for
// the three-state target_disk field: empty bytes, JSON null, an empty object,
// or whitespace around either. This is distinct from "nil raw" (= keep the
// existing override) which the caller checks beforehand.
func isClearJSON(raw []byte) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null" || s == "{}"
}
