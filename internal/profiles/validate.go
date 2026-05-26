package profiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// Profile-name and ID character classes. Names are operator-facing; IDs are
// the 32-hex form produced by newProfileID.
var (
	profileIDRE   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	profileNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

	// hostnameTemplateRE accepts RFC-1123 label characters plus the placeholder
	// syntax `{serial}`, `{uuid8}`, `{mac}`. Each label between dots must start
	// and end with alnum and may contain `-`. The full template length cap is
	// 253 chars (RFC 1035 §2.3.4), enforced separately below.
	hostnameTemplateRE = regexp.MustCompile(
		`^[A-Za-z0-9{]([A-Za-z0-9._{}-]*[A-Za-z0-9}])?$`)

	// sha512cryptRE matches the $6$ Linux crypt(3) format the agent will paste
	// straight into /etc/shadow. Salt is 1-16 [A-Za-z0-9./], hash is 86 [A-Za-z0-9./].
	sha512cryptRE = regexp.MustCompile(`^\$6\$(rounds=[0-9]+\$)?[A-Za-z0-9./]{1,16}\$[A-Za-z0-9./]{86}$`)

	// macRE matches AA:BB:CC:DD:EE:FF case-insensitive (we lowercase before storing).
	macRE = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)

	// validDiskModes is the closed set accepted by target_disk.mode.
	validDiskModes = map[string]bool{
		"smallest": true, "by-path": true, "by-wwn": true, "by-model": true,
	}
)

// Maximum lengths enforced at the API boundary.
const (
	MaxNameLen        = 64
	MaxDescriptionLen = 1024
	MaxHostnameLen    = 253
)

// TargetDisk encodes how the agent should pick the target disk on each
// machine. mode=smallest needs no value; the others require a matching
// /dev/disk/by-* identifier or model string.
type TargetDisk struct {
	Mode  string `json:"mode"`            // smallest | by-path | by-wwn | by-model
	Value string `json:"value,omitempty"` // empty iff mode=smallest
}

// NetworkConfig describes the network template baked into the profile. The
// per-machine static IP is *not* here — that lives on the binding so one
// profile can drive many machines. Fields that only matter when method=static
// (PrefixLen, Gateway, DNS) are still allowed when method=dhcp but ignored.
//
// When Bond is non-nil, NICSelector is meaningless and forced to "auto" on
// canonicalisation: the bond's Slaves field selects the underlying NICs
// directly by interface name, so the single-NIC selector logic no longer
// applies.
//
// VLAN is an optional 802.1Q tag (1..4094). When set, the IP config moves
// onto a VLAN sub-interface (metalkit0.<vlan> or bond0.<vlan>) and the
// underlying physical/bond device stays L2-only.
type NetworkConfig struct {
	Method      string      `json:"method"`                 // static | dhcp
	PrefixLen   int         `json:"prefix_len,omitempty"`
	Gateway     string      `json:"gateway,omitempty"`      // IPv4 literal
	DNS         []string    `json:"dns,omitempty"`          // IPv4 literals
	NICSelector string      `json:"nic_selector"`           // auto | by-mac:.. | by-name:..
	VLAN        int         `json:"vlan,omitempty"`         // 0 = none; 1..4094
	Bond        *BondConfig `json:"bond,omitempty"`         // when set, NICSelector ignored
}

// BondConfig is the per-profile network bonding template. Two modes are
// accepted: "active-backup" (works without any switch-side cooperation) and
// "802.3ad" (LACP — requires the upstream switch to be in a matching
// port-channel; metalkit does not configure the switch, that's on the
// operator per plan §F6).
//
// Slaves is the list of two-or-more underlying NIC interface names (e.g.
// "eno1", "eno2"). At install time the agent resolves each name to its MAC
// on the live system and emits a match.macaddress block in netplan; if the
// NIC list is unavailable it falls back to match.name. Using names keeps the
// profile template portable across machines — the same "eno1+eno2" bond
// profile works on any machine that has those interfaces.
type BondConfig struct {
	Mode           string   `json:"mode"`                       // active-backup | 802.3ad
	Slaves         []string `json:"slaves"`                     // 2..8 interface names
	Miimon         int      `json:"miimon,omitempty"`           // default 100; 50..10000
	LACPRate       string   `json:"lacp_rate,omitempty"`        // 802.3ad: slow|fast (default fast)
	XmitHashPolicy string   `json:"xmit_hash_policy,omitempty"` // 802.3ad: layer2|layer2+3|layer3+4 (default layer3+4)
	Primary        string   `json:"primary,omitempty"`          // active-backup only; interface name, must be in Slaves
}

// Valid bond mode / lacp_rate / xmit_hash_policy values.
var (
	validBondModes = map[string]bool{
		"active-backup": true,
		"802.3ad":       true,
	}
	validLACPRates = map[string]bool{
		"slow": true,
		"fast": true,
	}
	validXmitHashPolicies = map[string]bool{
		"layer2":   true,
		"layer2+3": true,
		"layer3+4": true,
	}
)

// ValidateTargetDisk parses+validates the JSON blob. Returns the
// canonicalised form (mode lowercased, value trimmed) for storage.
// Exported so bindings can validate per-binding target_disk overrides
// against the same rules.
func ValidateTargetDisk(raw json.RawMessage) (TargetDisk, error) {
	return validateTargetDisk(raw)
}

// ValidateBondJSON parses+validates the JSON blob as a BondConfig and
// returns the canonicalised form (lowercased MACs, defaulted miimon /
// lacp_rate / xmit_hash_policy). Exported so bindings can validate
// per-binding bond overrides against the same rules.
func ValidateBondJSON(raw json.RawMessage) (BondConfig, error) {
	var b BondConfig
	if len(raw) == 0 {
		return b, errors.New("bond is required")
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, fmt.Errorf("bond: %w", err)
	}
	if err := validateBond(&b); err != nil {
		return b, err
	}
	return b, nil
}

// validateTargetDisk parses+validates the JSON blob. Returns the
// canonicalised form (mode lowercased, value trimmed) for storage.
func validateTargetDisk(raw json.RawMessage) (TargetDisk, error) {
	var td TargetDisk
	if len(raw) == 0 {
		return td, errors.New("target_disk is required")
	}
	if err := json.Unmarshal(raw, &td); err != nil {
		return td, fmt.Errorf("target_disk: %w", err)
	}
	td.Mode = strings.ToLower(strings.TrimSpace(td.Mode))
	td.Value = strings.TrimSpace(td.Value)
	if !validDiskModes[td.Mode] {
		return td, fmt.Errorf("target_disk.mode %q: must be one of smallest/by-path/by-wwn/by-model", td.Mode)
	}
	if td.Mode == "smallest" {
		if td.Value != "" {
			return td, errors.New("target_disk.value must be empty when mode=smallest")
		}
		return td, nil
	}
	if td.Value == "" {
		return td, fmt.Errorf("target_disk.value is required for mode=%s", td.Mode)
	}
	return td, nil
}

// validateNetwork parses+validates the JSON network template.
func validateNetwork(raw json.RawMessage) (NetworkConfig, error) {
	var nc NetworkConfig
	if len(raw) == 0 {
		return nc, errors.New("network is required")
	}
	if err := json.Unmarshal(raw, &nc); err != nil {
		return nc, fmt.Errorf("network: %w", err)
	}
	nc.Method = strings.ToLower(strings.TrimSpace(nc.Method))
	switch nc.Method {
	case "static":
		if nc.PrefixLen < 1 || nc.PrefixLen > 32 {
			return nc, fmt.Errorf("network.prefix_len %d: must be 1..32", nc.PrefixLen)
		}
		if ip := net.ParseIP(nc.Gateway); ip == nil || ip.To4() == nil {
			return nc, fmt.Errorf("network.gateway %q: must be IPv4 literal", nc.Gateway)
		}
		for i, d := range nc.DNS {
			ip := net.ParseIP(strings.TrimSpace(d))
			if ip == nil || ip.To4() == nil {
				return nc, fmt.Errorf("network.dns[%d] %q: must be IPv4 literal", i, d)
			}
			nc.DNS[i] = ip.String()
		}
	case "dhcp":
		// Strip static-only fields so the persisted blob is canonical.
		nc.PrefixLen = 0
		nc.Gateway = ""
		nc.DNS = nil
	default:
		return nc, fmt.Errorf("network.method %q: must be static or dhcp", nc.Method)
	}
	nc.NICSelector = strings.TrimSpace(nc.NICSelector)
	if nc.NICSelector == "" {
		nc.NICSelector = "auto"
	}
	if err := validateNICSelector(nc.NICSelector); err != nil {
		return nc, err
	}
	if nc.VLAN != 0 && (nc.VLAN < 1 || nc.VLAN > 4094) {
		return nc, fmt.Errorf("network.vlan %d: must be 1..4094", nc.VLAN)
	}
	if nc.Bond != nil {
		if err := validateBond(nc.Bond); err != nil {
			return nc, err
		}
		// With a bond, the per-NIC selector is no longer used (the bond's
		// Slaves field selects the underlying NICs). Force it to "auto" so
		// the canonical persisted form is deterministic.
		nc.NICSelector = "auto"
	}
	return nc, nil
}

// validateBond enforces the bond schema and canonicalises the fields in
// place (lowercased MACs, default miimon/lacp_rate/xmit_hash_policy). It
// errors on any structural problem so the persisted blob is always
// well-formed.
func validateBond(b *BondConfig) error {
	b.Mode = strings.ToLower(strings.TrimSpace(b.Mode))
	if !validBondModes[b.Mode] {
		return fmt.Errorf("network.bond.mode %q: must be active-backup or 802.3ad", b.Mode)
	}

	// Slaves: 2..8 interface names, no dupes, canonicalised.
	if len(b.Slaves) < 2 {
		return fmt.Errorf("network.bond.slaves: need at least 2 interfaces (got %d)", len(b.Slaves))
	}
	if len(b.Slaves) > 8 {
		return fmt.Errorf("network.bond.slaves: at most 8 interfaces (got %d)", len(b.Slaves))
	}
	seen := make(map[string]struct{}, len(b.Slaves))
	for i, s := range b.Slaves {
		s = strings.TrimSpace(s)
		if err := validateIFName(s); err != nil {
			return fmt.Errorf("network.bond.slaves[%d] %q: %w", i, s, err)
		}
		if _, dup := seen[s]; dup {
			return fmt.Errorf("network.bond.slaves[%d] %q: duplicate interface name", i, s)
		}
		seen[s] = struct{}{}
		b.Slaves[i] = s
	}

	// Miimon: default 100, range 50..10000.
	if b.Miimon == 0 {
		b.Miimon = 100
	}
	if b.Miimon < 50 || b.Miimon > 10000 {
		return fmt.Errorf("network.bond.miimon %d: must be 50..10000 (or 0 for default 100)", b.Miimon)
	}

	switch b.Mode {
	case "active-backup":
		if b.LACPRate != "" {
			return errors.New("network.bond.lacp_rate must be empty when mode=active-backup")
		}
		if b.XmitHashPolicy != "" {
			return errors.New("network.bond.xmit_hash_policy must be empty when mode=active-backup")
		}
		if b.Primary != "" {
			b.Primary = strings.TrimSpace(b.Primary)
			if err := validateIFName(b.Primary); err != nil {
				return fmt.Errorf("network.bond.primary %q: %w", b.Primary, err)
			}
			if _, ok := seen[b.Primary]; !ok {
				return fmt.Errorf("network.bond.primary %q: must be one of slaves", b.Primary)
			}
		}
	case "802.3ad":
		if b.Primary != "" {
			return errors.New("network.bond.primary must be empty when mode=802.3ad")
		}
		b.LACPRate = strings.ToLower(strings.TrimSpace(b.LACPRate))
		if b.LACPRate == "" {
			b.LACPRate = "fast"
		}
		if !validLACPRates[b.LACPRate] {
			return fmt.Errorf("network.bond.lacp_rate %q: must be slow or fast", b.LACPRate)
		}
		b.XmitHashPolicy = strings.ToLower(strings.TrimSpace(b.XmitHashPolicy))
		if b.XmitHashPolicy == "" {
			b.XmitHashPolicy = "layer3+4"
		}
		if !validXmitHashPolicies[b.XmitHashPolicy] {
			return fmt.Errorf("network.bond.xmit_hash_policy %q: must be layer2|layer2+3|layer3+4", b.XmitHashPolicy)
		}
	}
	return nil
}

func validateNICSelector(sel string) error {
	switch {
	case sel == "auto":
		return nil
	case strings.HasPrefix(sel, "by-mac:"):
		mac := strings.TrimPrefix(sel, "by-mac:")
		if !macRE.MatchString(mac) {
			return fmt.Errorf("network.nic_selector %q: bad MAC after by-mac:", sel)
		}
		return nil
	case strings.HasPrefix(sel, "by-name:"):
		name := strings.TrimPrefix(sel, "by-name:")
		if err := validateIFName(name); err != nil {
			return fmt.Errorf("network.nic_selector %q: %w", sel, err)
		}
		return nil
	default:
		return fmt.Errorf("network.nic_selector %q: must be auto|by-mac:..|by-name:..", sel)
	}
}

// validateIFName checks that s is a valid Linux interface name: 1–15 chars,
// no whitespace, no slash. Used for bond slaves and by-name NIC selectors.
func validateIFName(s string) error {
	if s == "" || len(s) > 15 || strings.ContainsAny(s, " /\t\n") {
		return fmt.Errorf("invalid interface name %q (1-15 chars, no space/slash)", s)
	}
	return nil
}

// validateHostnameTemplate enforces the regex *and* the 253-char cap. The
// agent does placeholder substitution at install time; we just check the
// shape here.
func validateHostnameTemplate(t string) error {
	t = strings.TrimSpace(t)
	if t == "" {
		return errors.New("hostname_template is required")
	}
	if len(t) > MaxHostnameLen {
		return fmt.Errorf("hostname_template length %d exceeds %d", len(t), MaxHostnameLen)
	}
	if !hostnameTemplateRE.MatchString(t) {
		return fmt.Errorf("hostname_template %q: invalid characters", t)
	}
	// Each label between dots must be ≤ 63 chars (RFC 1035). Placeholders
	// {serial}/{uuid8}/{mac} expand to at most ~32 chars; allow them through.
	for _, label := range strings.Split(t, ".") {
		if len(label) > 63 && !strings.ContainsAny(label, "{}") {
			return fmt.Errorf("hostname_template label %q exceeds 63 chars", label)
		}
	}
	return nil
}
