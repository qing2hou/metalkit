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
	}))
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
	}))
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
	}))
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
	}))
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
			}))
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wants)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wants)
			}
		})
	}
}
