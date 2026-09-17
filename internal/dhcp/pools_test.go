package dhcp

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

func mkReq(t *testing.T, giaddr string) *dhcpv4.DHCPv4 {
	t.Helper()
	req, err := dhcpv4.NewDiscovery(net.HardwareAddr{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01})
	if err != nil {
		t.Fatal(err)
	}
	if giaddr != "" {
		req.GatewayIPAddr = netip.MustParseAddr(giaddr).AsSlice()
	}
	return req
}

func TestSubnetPoolsRelayRouting(t *testing.T) {
	pools, err := NewSubnetPools([]SubnetPoolInput{
		{Start: "192.168.10.100", End: "192.168.10.200", Netmask: "255.255.255.0", Gateway: "192.168.10.1", LeaseSec: 3600},
		{Selector: "192.168.1.0/24", Start: "192.168.1.100", End: "192.168.1.200", Netmask: "255.255.255.0", Gateway: "192.168.1.10", LeaseSec: 3600},
	})
	if err != nil {
		t.Fatalf("NewSubnetPools: %v", err)
	}
	srv := &Server{logger: testLogger(), srvIP: netip.MustParseAddr("192.168.10.11").AsSlice()}

	// Relayed DISCOVER from the 192.168.1.x SVI must offer from that pool.
	fake := &fakeLeaseStore{allocIP: "192.168.1.100"}
	srv.Reload(ModeFull, pools, fake)
	req := mkReq(t, "192.168.1.10")
	reply, err := srv.buildFullReply(context.Background(), req)
	if err != nil || reply == nil {
		t.Fatalf("relay reply: %v %v", reply, err)
	}
	if reply.YourIPAddr.String() != "192.168.1.100" {
		t.Errorf("relay yiaddr = %s, want 192.168.1.100", reply.YourIPAddr)
	}
	if gw := reply.GetOneOption(dhcpv4.OptionRouter); string(gw) != "\xc0\xa8\x01\x0a" {
		t.Errorf("relay router option = %x, want 192.168.1.10", gw)
	}

	// Direct client falls back to the local pool.
	req2 := mkReq(t, "")
	fake2 := &fakeLeaseStore{allocIP: "192.168.10.150"}
	srv.Reload(ModeFull, pools, fake2)
	reply2, err := srv.buildFullReply(context.Background(), req2)
	if err != nil || reply2 == nil {
		t.Fatalf("direct reply: %v %v", reply2, err)
	}
	if reply2.YourIPAddr.String() != "192.168.10.150" {
		t.Errorf("direct yiaddr = %s, want 192.168.10.150", reply2.YourIPAddr)
	}

	// Unknown relay subnet: no pool, no answer.
	req3 := mkReq(t, "172.16.99.1")
	reply3, err := srv.buildFullReply(context.Background(), req3)
	if err != nil {
		t.Fatalf("unknown relay: %v", err)
	}
	if reply3 != nil {
		t.Errorf("unknown giaddr got reply yiaddr=%s, want silence", reply3.YourIPAddr)
	}
}

func TestSubnetPoolsValidation(t *testing.T) {
	local := SubnetPoolInput{Start: "10.0.0.1", End: "10.0.0.50", Netmask: "255.255.255.0", Gateway: "10.0.0.254"}
	// No local pool.
	if _, err := NewSubnetPools([]SubnetPoolInput{
		{Selector: "192.168.1.0/24", Start: "192.168.1.10", End: "192.168.1.20", Netmask: "255.255.255.0", Gateway: "192.168.1.10"},
	}); err == nil {
		t.Error("missing local pool accepted")
	}
	// Two local pools.
	if _, err := NewSubnetPools([]SubnetPoolInput{local, local}); err == nil {
		t.Error("duplicate local pool accepted")
	}
	// Duplicate selector.
	if _, err := NewSubnetPools([]SubnetPoolInput{local,
		{Selector: "192.168.1.0/24", Start: "192.168.1.10", End: "192.168.1.20", Netmask: "255.255.255.0", Gateway: "192.168.1.10"},
		{Selector: "192.168.1.10", Start: "192.168.1.30", End: "192.168.1.40", Netmask: "255.255.255.0", Gateway: "192.168.1.10"},
	}); err == nil {
		t.Error("duplicate selector accepted")
	}
	// Selector outside its pool network.
	if _, err := NewSubnetPools([]SubnetPoolInput{local,
		{Selector: "192.168.1.1/24", Start: "10.0.0.60", End: "10.0.0.70", Netmask: "255.255.255.0", Gateway: "10.0.0.254"},
	}); err == nil {
		t.Error("selector outside pool network accepted")
	}
}

func TestSubnetPoolsMultiRange(t *testing.T) {
	pools, err := NewSubnetPools([]SubnetPoolInput{
		{Pools: []PoolRange{{Start: "192.168.10.150", End: "192.168.10.200"}}, Netmask: "255.255.255.0", Gateway: "192.168.10.1", LeaseSec: 3600},
		{Selector: "192.168.1.0/24",
			Pools: []PoolRange{
				{Start: "192.168.1.100", End: "192.168.1.101"},
				{Start: "192.168.1.180", End: "192.168.1.181"},
			},
			Netmask: "255.255.255.0", Gateway: "192.168.1.10", LeaseSec: 3600},
	})
	if err != nil {
		t.Fatalf("NewSubnetPools: %v", err)
	}
	relay := pools.poolForRequest(netip.MustParseAddr("192.168.1.10"))
	if relay == nil || len(relay.Ranges) != 2 {
		t.Fatalf("relay pool ranges = %d, want 2", len(relay.Ranges))
	}
	// Contains honours both segments.
	for ip, want := range map[string]bool{
		"192.168.1.100": true, "192.168.1.101": true,
		"192.168.1.102": false, // gap between segments
		"192.168.1.180": true, "192.168.1.182": false,
	} {
		if got := relay.Contains(netip.MustParseAddr(ip)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", ip, got, want)
		}
	}
	// Overlapping segments are rejected at construction.
	if _, err := NewSubnetPools([]SubnetPoolInput{
		{Pools: []PoolRange{{Start: "10.0.0.1", End: "10.0.0.10"}}, Netmask: "255.255.255.0", Gateway: "10.0.0.254"},
		{Selector: "192.168.1.0/24", Netmask: "255.255.255.0", Gateway: "192.168.1.10",
			Pools: []PoolRange{{Start: "192.168.1.10", End: "192.168.1.20"}, {Start: "192.168.1.15", End: "192.168.1.25"}}},
	}); err == nil {
		t.Error("overlapping ranges accepted")
	}
}
