package bmc

import (
	"fmt"
	"net"
	"strings"
)

// PlaceholderPrefix marks BMC-only rows that don't yet correspond to a real
// machine. When a BMC is registered before its host PXE-boots and reports a
// SMBIOS UUID, we insert a placeholder row in machines(uuid) so the FK on
// bmc_credentials resolves; the row is later reconciled by inventory.UpsertReport
// (which migrates the BMC ownership to the real SMBIOS UUID and deletes the
// placeholder). See ReconcilePlaceholder below.
const PlaceholderPrefix = "placeholder-"

// PlaceholderUUID derives a deterministic placeholder UUID from a BMC IP. The
// format is `placeholder-<ipv4-with-dots-as-dashes>`, e.g.
//
//	192.168.10.254 → placeholder-192-168-10-254
//
// IP is parsed and normalized first so different textual forms (10.0.0.1 vs
// 010.000.000.001) collapse to the same key. Returns "" if the IP is invalid.
func PlaceholderUUID(ip string) string {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return ""
	}
	v4 := parsed.To4()
	if v4 == nil {
		return ""
	}
	return PlaceholderPrefix + strings.ReplaceAll(v4.String(), ".", "-")
}

// IsPlaceholderUUID reports whether s is a placeholder UUID produced by
// PlaceholderUUID. Used by the store to decide whether it may auto-create the
// machine row, and by the UI to render the row as "待对账" (pending reconcile).
func IsPlaceholderUUID(s string) bool {
	return strings.HasPrefix(s, PlaceholderPrefix)
}

// validatePlaceholderUUID asserts s is a syntactically valid placeholder UUID.
// Returns the normalized form. Distinct from validateMachineUUID, which only
// accepts canonical SMBIOS UUIDs.
func validatePlaceholderUUID(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !IsPlaceholderUUID(s) {
		return "", fmt.Errorf("machine_uuid %q: not a placeholder UUID", s)
	}
	suffix := strings.TrimPrefix(s, PlaceholderPrefix)
	ip := strings.ReplaceAll(suffix, "-", ".")
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return "", fmt.Errorf("machine_uuid %q: placeholder suffix %q is not a valid IPv4", s, suffix)
	}
	// Re-derive from the parsed IP so 10.0.0.1 vs 010.000.000.001 normalize.
	return PlaceholderUUID(parsed.String()), nil
}
