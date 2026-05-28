package subnets

import (
	"net/netip"
	"strings"
	"testing"
)

func TestValidateCIDR(t *testing.T) {
	cases := []struct {
		in        string
		canon     string
		wantErr   bool
		errSubstr string
	}{
		{"192.168.10.0/24", "192.168.10.0/24", false, ""},
		{"  192.168.10.0/24  ", "192.168.10.0/24", false, ""},
		// host bits get masked into the network form
		{"192.168.10.50/24", "192.168.10.0/24", false, ""},
		{"10.0.0.0/8", "10.0.0.0/8", false, ""},
		// /32 still legal — single-host subnet
		{"10.0.0.1/32", "10.0.0.1/32", false, ""},
		{"", "", true, "cidr"},
		{"not-a-cidr", "", true, "cidr"},
		{"::1/128", "", true, "IPv4 only"},
		{"192.168.10.0/33", "", true, "cidr"},
		{"192.168.10.0/0", "", true, "prefix length"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, _, err := validateCIDR(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (canon=%q)", got)
				}
				if !strings.Contains(err.Error(), c.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), c.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.canon {
				t.Errorf("canon = %q, want %q", got, c.canon)
			}
		})
	}
}

func TestValidateGateway(t *testing.T) {
	prefix := netip.MustParsePrefix("192.168.10.0/24")
	cases := []struct {
		in        string
		want      string
		wantErr   bool
		errSubstr string
	}{
		{"192.168.10.1", "192.168.10.1", false, ""},
		{"192.168.10.254", "192.168.10.254", false, ""},
		{"  192.168.10.1  ", "192.168.10.1", false, ""},
		// network address rejected
		{"192.168.10.0", "", true, "network"},
		// broadcast address rejected
		{"192.168.10.255", "", true, "broadcast"},
		{"10.0.0.1", "", true, "not inside cidr"},
		{"::1", "", true, "IPv4 only"},
		{"junk", "", true, "gateway"},
		{"", "", true, "gateway"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := validateGateway(c.in, prefix)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				if !strings.Contains(err.Error(), c.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), c.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got = %q, want %q", got, c.want)
			}
		})
	}
}

func TestValidateGatewaySlash31(t *testing.T) {
	// /31 has no broadcast — both addresses are usable.
	prefix := netip.MustParsePrefix("10.0.0.0/31")
	got, err := validateGateway("10.0.0.1", prefix)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "10.0.0.1" {
		t.Errorf("got = %q, want 10.0.0.1", got)
	}
}

func TestValidateDNS(t *testing.T) {
	got, err := validateDNS([]string{"1.1.1.1", "  8.8.8.8  ", "1.1.1.1", "", "9.9.9.9"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i, v := range got {
		if v != want[i] {
			t.Errorf("[%d] = %q, want %q", i, v, want[i])
		}
	}
}

func TestValidateDNSReject(t *testing.T) {
	cases := []struct {
		in        []string
		errSubstr string
	}{
		{[]string{"not.an.ip"}, "dns"},
		{[]string{"::1"}, "IPv4 only"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.in, ","), func(t *testing.T) {
			_, err := validateDNS(c.in)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.errSubstr)
			}
			if !strings.Contains(err.Error(), c.errSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), c.errSubstr)
			}
		})
	}
}

func TestValidateVLAN(t *testing.T) {
	for _, ok := range []int{0, 1, 100, 4094} {
		if err := validateVLAN(ok); err != nil {
			t.Errorf("validateVLAN(%d) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []int{-1, 4095, 100000} {
		if err := validateVLAN(bad); err == nil {
			t.Errorf("validateVLAN(%d): expected error, got nil", bad)
		}
	}
}

func TestHostInSubnet(t *testing.T) {
	cases := []struct {
		ip, cidr, gw string
		wantErr      bool
		errSubstr    string
	}{
		{"192.168.10.50", "192.168.10.0/24", "192.168.10.1", false, ""},
		// equals gateway -> reject
		{"192.168.10.1", "192.168.10.0/24", "192.168.10.1", true, "equals gateway"},
		// network / broadcast rejected
		{"192.168.10.0", "192.168.10.0/24", "192.168.10.1", true, "network"},
		{"192.168.10.255", "192.168.10.0/24", "192.168.10.1", true, "broadcast"},
		// outside prefix
		{"10.0.0.5", "192.168.10.0/24", "192.168.10.1", true, "not inside"},
		// bad cidr propagates
		{"192.168.10.50", "bogus", "", true, "cidr"},
		// bad ip
		{"junk", "192.168.10.0/24", "", true, "host ip"},
		// empty gateway means no equality check (still ok)
		{"192.168.10.50", "192.168.10.0/24", "", false, ""},
	}
	for _, c := range cases {
		t.Run(c.ip, func(t *testing.T) {
			err := HostInSubnet(c.ip, c.cidr, c.gw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), c.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), c.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestBroadcastOf(t *testing.T) {
	cases := []struct {
		cidr string
		want string // empty string means invalid
	}{
		{"192.168.10.0/24", "192.168.10.255"},
		{"10.0.0.0/8", "10.255.255.255"},
		{"192.168.0.0/16", "192.168.255.255"},
		{"10.0.0.0/30", "10.0.0.3"},
		// /31 and /32 have no broadcast concept
		{"10.0.0.0/31", ""},
		{"10.0.0.1/32", ""},
	}
	for _, c := range cases {
		t.Run(c.cidr, func(t *testing.T) {
			p := netip.MustParsePrefix(c.cidr)
			got := broadcastOf(p)
			if c.want == "" {
				if got.IsValid() {
					t.Errorf("expected invalid, got %v", got)
				}
				return
			}
			if got.String() != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
