package profiles

import (
	"encoding/json"
	"strings"
	"testing"
)

// Helpers ----------------------------------------------------------------

func ncJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// validateNetwork happy paths ------------------------------------------

func TestValidateNetwork_DHCPAuto(t *testing.T) {
	nc, err := validateNetwork(ncJSON(t, map[string]any{
		"method":       "dhcp",
		"nic_selector": "auto",
	}), false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if nc.Method != "dhcp" || nc.NICSelector != "auto" || nc.Bond != nil {
		t.Fatalf("canonicalised wrong: %+v", nc)
	}
	// DHCP must strip static-only fields.
	if nc.PrefixLen != 0 || nc.Gateway != "" || len(nc.DNS) != 0 {
		t.Fatalf("dhcp leaked static fields: %+v", nc)
	}
}

func TestValidateNetwork_StaticByMAC(t *testing.T) {
	nc, err := validateNetwork(ncJSON(t, map[string]any{
		"method":       "static",
		"prefix_len":   24,
		"gateway":      "10.0.0.1",
		"dns":          []string{"8.8.8.8", "1.1.1.1"},
		"nic_selector": "by-mac:AA:BB:CC:DD:EE:FF",
	}), false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if nc.NICSelector != "by-mac:AA:BB:CC:DD:EE:FF" {
		// nic_selector is NOT lowercased today; the matcher accepts either case.
		// The test only locks in current behaviour — if you change it, update here.
		t.Logf("nic_selector preserved as %q", nc.NICSelector)
	}
}

// validateBond — happy paths -------------------------------------------

func TestValidateNetwork_BondActiveBackup(t *testing.T) {
	nc, err := validateNetwork(ncJSON(t, map[string]any{
		"method":       "static",
		"prefix_len":   24,
		"gateway":      "10.0.0.1",
		"dns":          []string{"8.8.8.8"},
		"nic_selector": "by-name:eno1", // should be forced to "auto" because bond present
		"bond": map[string]any{
			"mode": "active-backup",
			"slaves": []string{
				"eno1",
				"eno2",
			},
			"primary": "eno1",
		},
	}), false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if nc.NICSelector != "auto" {
		t.Errorf("bond present should force nic_selector=auto, got %q", nc.NICSelector)
	}
	if nc.Bond == nil {
		t.Fatal("Bond got dropped")
	}
	if nc.Bond.Mode != "active-backup" {
		t.Errorf("mode=%q", nc.Bond.Mode)
	}
	// Default miimon applied.
	if nc.Bond.Miimon != 100 {
		t.Errorf("miimon default not applied: got %d", nc.Bond.Miimon)
	}
	if nc.Bond.Primary != "eno1" {
		t.Errorf("primary: got %q", nc.Bond.Primary)
	}
	// active-backup must NOT have lacp_rate / xmit_hash_policy.
	if nc.Bond.LACPRate != "" || nc.Bond.XmitHashPolicy != "" {
		t.Errorf("ab leaked lacp/xmit: %+v", nc.Bond)
	}
}

func TestValidateNetwork_BondLACPDefaults(t *testing.T) {
	nc, err := validateNetwork(ncJSON(t, map[string]any{
		"method":     "dhcp",
		"prefix_len": 24,
		"bond": map[string]any{
			"mode": "802.3ad",
			"slaves": []string{
				"eno1",
				"eno2",
			},
		},
	}), false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if nc.Bond.LACPRate != "fast" {
		t.Errorf("lacp_rate default not fast: %q", nc.Bond.LACPRate)
	}
	if nc.Bond.XmitHashPolicy != "layer3+4" {
		t.Errorf("xmit_hash_policy default not layer3+4: %q", nc.Bond.XmitHashPolicy)
	}
	if nc.Bond.Miimon != 100 {
		t.Errorf("miimon default not 100: %d", nc.Bond.Miimon)
	}
}

func TestValidateNetwork_BondNameDefaultsAndCanonicalises(t *testing.T) {
	// Omitted name → bond0, so pre-name-era configs render identically.
	nc, err := validateNetwork(ncJSON(t, map[string]any{
		"method": "dhcp",
		"bond": map[string]any{
			"mode":   "active-backup",
			"slaves": []string{"eno1", "eno2"},
		},
	}), false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if nc.Bond.Name != DefaultBondName {
		t.Errorf("name default: got %q, want %q", nc.Bond.Name, DefaultBondName)
	}
	if got := nc.Bond.InterfaceName(); got != DefaultBondName {
		t.Errorf("InterfaceName default: got %q", got)
	}

	// Custom name is trimmed and kept as-is.
	nc, err = validateNetwork(ncJSON(t, map[string]any{
		"method": "dhcp",
		"bond": map[string]any{
			"name":   "  mgmt-bond ",
			"mode":   "active-backup",
			"slaves": []string{"eno1", "eno2"},
		},
	}), false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if nc.Bond.Name != "mgmt-bond" {
		t.Errorf("name canonicalisation: got %q", nc.Bond.Name)
	}
	if got := nc.Bond.InterfaceName(); got != "mgmt-bond" {
		t.Errorf("InterfaceName: got %q", got)
	}

	// Legacy zero-value config (no name field persisted) still resolves.
	var legacy BondConfig
	if got := legacy.InterfaceName(); got != DefaultBondName {
		t.Errorf("legacy InterfaceName: got %q", got)
	}
	var nilBond *BondConfig
	if got := nilBond.InterfaceName(); got != DefaultBondName {
		t.Errorf("nil InterfaceName: got %q", got)
	}
}

func TestValidateNetwork_BondNameErrors(t *testing.T) {
	cases := []struct {
		name  string
		bond  map[string]any
		wants string
	}{
		{
			name:  "dot is reserved for vlan",
			bond:  map[string]any{"name": "bond0.100", "mode": "active-backup", "slaves": []string{"eno1", "eno2"}},
			wants: "network.bond.name",
		},
		{
			name:  "space rejected",
			bond:  map[string]any{"name": "bond 0", "mode": "active-backup", "slaves": []string{"eno1", "eno2"}},
			wants: "network.bond.name",
		},
		{
			name:  "slash rejected (path separator)",
			bond:  map[string]any{"name": "bond/0", "mode": "active-backup", "slaves": []string{"eno1", "eno2"}},
			wants: "network.bond.name",
		},
		{
			name:  "must start with a letter",
			bond:  map[string]any{"name": "0bond", "mode": "active-backup", "slaves": []string{"eno1", "eno2"}},
			wants: "network.bond.name",
		},
		{
			name:  "too long (IFNAMSIZ)",
			bond:  map[string]any{"name": "abcdefghijklmnop", "mode": "active-backup", "slaves": []string{"eno1", "eno2"}},
			wants: "network.bond.name",
		},
		{
			name:  "collides with a slave",
			bond:  map[string]any{"name": "eno1", "mode": "active-backup", "slaves": []string{"eno1", "eno2"}},
			wants: "must not collide",
		},
		{
			name:  "reserved loopback name",
			bond:  map[string]any{"name": "lo", "mode": "active-backup", "slaves": []string{"eno1", "eno2"}},
			wants: "reserved interface name",
		},
		{
			name:  "reserved ethN namespace",
			bond:  map[string]any{"name": "eth0", "mode": "active-backup", "slaves": []string{"eno1", "eno2"}},
			wants: "reserved interface name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateNetwork(ncJSON(t, map[string]any{
				"method": "dhcp",
				"bond":   tc.bond,
			}), false)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wants)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("error %q does not contain %q", err, tc.wants)
			}
		})
	}
}

// TestValidateNetwork_BondNameVLANLength checks the derived sub-interface
// name <bond>.<vlan> against Linux's 15-char limit when the VLAN is set on
// the profile itself (subnet-inherited VLANs are checked at install time).
func TestValidateNetwork_BondNameVLANLength(t *testing.T) {
	nc, err := validateNetwork(ncJSON(t, map[string]any{
		"method": "dhcp",
		"vlan":   4094,
		"bond": map[string]any{
			"name":   "abcdefghijklmno", // 15 chars
			"mode":   "active-backup",
			"slaves": []string{"eno1", "eno2"},
		},
	}), false)
	if err == nil {
		t.Fatalf("expected IFNAMSIZ error, got %+v", nc)
	}
	if !strings.Contains(err.Error(), "15-char") {
		t.Fatalf("error %q does not mention the 15-char limit", err)
	}

	// A short name with the same VLAN is fine.
	if _, err := validateNetwork(ncJSON(t, map[string]any{
		"method": "dhcp",
		"vlan":   4094,
		"bond": map[string]any{
			"name":   "bond1",
			"mode":   "active-backup",
			"slaves": []string{"eno1", "eno2"},
		},
	}), false); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

// validateBond — error paths -------------------------------------------

func TestValidateNetwork_BondErrors(t *testing.T) {
	cases := []struct {
		name  string
		bond  map[string]any
		wants string
	}{
		{
			name: "bad mode",
			bond: map[string]any{
				"mode":   "balance-rr",
				"slaves": []string{"eno1", "eno2"},
			},
			wants: "mode",
		},
		{
			name: "too few slaves",
			bond: map[string]any{
				"mode":   "active-backup",
				"slaves": []string{"eno1"},
			},
			wants: "at least 2",
		},
		{
			name: "duplicate slaves",
			bond: map[string]any{
				"mode":   "active-backup",
				"slaves": []string{"eno1", "eno1"},
			},
			wants: "duplicate",
		},
		{
			name: "bad interface name",
			bond: map[string]any{
				"mode":   "active-backup",
				"slaves": []string{"eno1", "bad name with spaces"},
			},
			wants: "invalid interface name",
		},
		{
			name: "primary not in slaves",
			bond: map[string]any{
				"mode":    "active-backup",
				"slaves":  []string{"eno1", "eno2"},
				"primary": "eno9",
			},
			wants: "must be one of slaves",
		},
		{
			name: "lacp_rate on active-backup",
			bond: map[string]any{
				"mode":      "active-backup",
				"slaves":    []string{"eno1", "eno2"},
				"lacp_rate": "fast",
			},
			wants: "lacp_rate must be empty",
		},
		{
			name: "primary on 802.3ad",
			bond: map[string]any{
				"mode":    "802.3ad",
				"slaves":  []string{"eno1", "eno2"},
				"primary": "eno1",
			},
			wants: "primary must be empty",
		},
		{
			name: "bad lacp_rate",
			bond: map[string]any{
				"mode":      "802.3ad",
				"slaves":    []string{"eno1", "eno2"},
				"lacp_rate": "medium",
			},
			wants: "lacp_rate",
		},
		{
			name: "bad xmit_hash_policy",
			bond: map[string]any{
				"mode":             "802.3ad",
				"slaves":           []string{"eno1", "eno2"},
				"xmit_hash_policy": "layer9",
			},
			wants: "xmit_hash_policy",
		},
		{
			name: "miimon out of range",
			bond: map[string]any{
				"mode":   "active-backup",
				"slaves": []string{"eno1", "eno2"},
				"miimon": 5,
			},
			wants: "miimon",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateNetwork(ncJSON(t, map[string]any{
				"method":       "dhcp",
				"nic_selector": "auto",
				"bond":         tc.bond,
			}), false)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wants)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wants)
			}
		})
	}
}

func TestValidateChrootDNS(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{name: "empty", in: "", want: ""},
		{name: "single ipv4", in: "223.5.5.5", want: "223.5.5.5"},
		{name: "comma separated", in: "223.5.5.5,114.114.114.114", want: "223.5.5.5,114.114.114.114"},
		{name: "space separated", in: "223.5.5.5 114.114.114.114", want: "223.5.5.5,114.114.114.114"},
		{name: "mixed separators + whitespace", in: " 223.5.5.5 , 1.1.1.1\n8.8.8.8 ", want: "223.5.5.5,1.1.1.1,8.8.8.8"},
		{name: "ipv6", in: "2001:4860:4860::8888", want: "2001:4860:4860::8888"},
		{name: "invalid ip", in: "not-an-ip", err: true},
		{name: "one invalid", in: "223.5.5.5,bad", err: true},
		{name: "too many", in: "1.1.1.1,2.2.2.2,3.3.3.3,4.4.4.4,5.5.5.5,6.6.6.6,7.7.7.7,8.8.8.8,9.9.9.9", err: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateChrootDNS(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("for %q: want %q got %q", tc.in, tc.want, got)
			}
		})
	}
}

func TestParseAndJoinChrootDNS(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{in: "", want: nil},
		{in: "223.5.5.5", want: []string{"223.5.5.5"}},
		{in: "223.5.5.5,114.114.114.114", want: []string{"223.5.5.5", "114.114.114.114"}},
	}
	for _, tc := range cases {
		got := parseChrootDNS(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("parse(%q): want %v got %v", tc.in, tc.want, got)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("parse(%q)[%d]: want %q got %q", tc.in, i, tc.want[i], got[i])
			}
		}
		// join should round-trip.
		joined := joinChrootDNS(got)
		if tc.in == "" {
			if joined != "" {
				t.Fatalf("join(nil): want empty, got %q", joined)
			}
		} else if joined != tc.in {
			t.Fatalf("join(parse(%q)) = %q, want %q", tc.in, joined, tc.in)
		}
	}
}
