package bmc

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// Canonical SMBIOS UUID form, same as inventory/profiles/bindings.
var smbiosUUIDRE = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IPMI usernames per spec are ≤16 chars ASCII, but vendors are loose; allow
// printable ASCII no whitespace up to 64 to be forgiving.
var usernameRE = regexp.MustCompile(`^[!-~]{1,64}$`)

// Optional display alias. Allow letters, digits, dot, underscore, dash, and
// CJK/printable Unicode through — the field is purely cosmetic for the UI.
// Length cap matches what bindings/profiles use elsewhere.
const maxNameLen = 64

var validInterfaces = map[string]bool{
	"lan":     true,
	"lanplus": true,
}

// MaxPasswordLen caps password length. IPMI 2.0 spec is 20 bytes; vendors
// sometimes accept 16. We allow up to 255 because some BMCs accept longer
// strings and we don't want to reject keystrokes that work.
const MaxPasswordLen = 255

func validateMachineUUID(u string) (string, error) {
	u = strings.ToLower(strings.TrimSpace(u))
	if !smbiosUUIDRE.MatchString(u) {
		return "", fmt.Errorf("machine_uuid %q: must be canonical lowercase SMBIOS UUID", u)
	}
	return u, nil
}

// validateMachineUUIDOrPlaceholder is the BMC-specific accepter: it allows
// either a canonical SMBIOS UUID (real, agent-reported machine) or a placeholder
// UUID produced by PlaceholderUUID (BMC registered before its host has PXE'd).
// Used by every Store entry point so a placeholder credential is a first-class
// object — readable, listable, deletable — until reconciliation migrates it.
func validateMachineUUIDOrPlaceholder(u string) (string, error) {
	u = strings.ToLower(strings.TrimSpace(u))
	if smbiosUUIDRE.MatchString(u) {
		return u, nil
	}
	if IsPlaceholderUUID(u) {
		return validatePlaceholderUUID(u)
	}
	return "", fmt.Errorf("machine_uuid %q: must be SMBIOS UUID or placeholder-<ipv4-dashed>", u)
}

func validateIP(ip string) (string, error) {
	ip = strings.TrimSpace(ip)
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return "", fmt.Errorf("ip %q: must be IPv4 literal", ip)
	}
	return parsed.String(), nil
}

func validatePort(p int) (int, error) {
	if p == 0 {
		return 623, nil
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("port %d: must be 1..65535", p)
	}
	return p, nil
}

func validateUsername(u string) (string, error) {
	u = strings.TrimSpace(u)
	if !usernameRE.MatchString(u) {
		return "", fmt.Errorf("username %q: must be 1..64 printable ASCII, no whitespace", u)
	}
	return u, nil
}

func validatePassword(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("password: required")
	}
	if len(p) > MaxPasswordLen {
		return "", fmt.Errorf("password length %d exceeds %d", len(p), MaxPasswordLen)
	}
	return p, nil
}

func validateInterface(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "lanplus", nil
	}
	if !validInterfaces[s] {
		return "", fmt.Errorf("ipmi_interface %q: must be lan or lanplus", s)
	}
	return s, nil
}

// validateName trims and length-caps the optional display alias. Empty is
// allowed (means "no alias, fall back to UUID in the UI"). Control characters
// are rejected to keep terminal/log output sane.
func validateName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" {
		return "", nil
	}
	if len([]rune(n)) > maxNameLen {
		return "", fmt.Errorf("name length %d exceeds %d runes", len([]rune(n)), maxNameLen)
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("name %q: contains control characters", n)
		}
	}
	return n, nil
}
