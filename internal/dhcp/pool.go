package dhcp

import (
	"fmt"
	"net/netip"
)

// Pool is the parsed/validated DHCP IP pool used in full mode. It is
// constructed from config.DHCPPool at startup and held by reference on
// Server. start/end are inclusive bounds; exclude is the set of host IPs
// we must never hand out (e.g. controller's serverIP, the gateway).
type Pool struct {
	Start    netip.Addr
	End      netip.Addr
	Netmask  netip.Addr // 255.255.255.0 etc — passed in option 1
	Gateway  netip.Addr // option 3
	DNS      []netip.Addr
	Exclude  map[string]struct{}
	LeaseSec uint32 // option 51
}

// NewPool validates each input and returns a Pool ready for the server to
// hand out IPs from. Inputs are strings so the config layer can stay free
// of net/netip in its YAML schema; the server constructs the Pool once.
func NewPool(start, end, netmask, gateway string, dns []string, leaseSec uint32, exclude []string) (*Pool, error) {
	startA, err := netip.ParseAddr(start)
	if err != nil || !startA.Is4() {
		return nil, fmt.Errorf("pool start %q: must be IPv4", start)
	}
	endA, err := netip.ParseAddr(end)
	if err != nil || !endA.Is4() {
		return nil, fmt.Errorf("pool end %q: must be IPv4", end)
	}
	if startA.Compare(endA) > 0 {
		return nil, fmt.Errorf("pool start %s > end %s", startA, endA)
	}
	maskA, err := netip.ParseAddr(netmask)
	if err != nil || !maskA.Is4() {
		return nil, fmt.Errorf("netmask %q: must be IPv4", netmask)
	}
	gwA, err := netip.ParseAddr(gateway)
	if err != nil || !gwA.Is4() {
		return nil, fmt.Errorf("gateway %q: must be IPv4", gateway)
	}
	dnsA := make([]netip.Addr, 0, len(dns))
	for _, d := range dns {
		a, err := netip.ParseAddr(d)
		if err != nil || !a.Is4() {
			return nil, fmt.Errorf("dns %q: must be IPv4", d)
		}
		dnsA = append(dnsA, a)
	}
	exSet := make(map[string]struct{}, len(exclude))
	for _, e := range exclude {
		a, err := netip.ParseAddr(e)
		if err != nil || !a.Is4() {
			return nil, fmt.Errorf("exclude %q: must be IPv4", e)
		}
		exSet[a.String()] = struct{}{}
	}
	if leaseSec == 0 {
		leaseSec = 24 * 3600
	}
	return &Pool{
		Start: startA, End: endA, Netmask: maskA, Gateway: gwA,
		DNS: dnsA, Exclude: exSet, LeaseSec: leaseSec,
	}, nil
}

// Contains reports whether ip is inside [start,end]. Used to validate a
// client's REQUEST: a REQUEST for an IP outside our pool gets NAK'd.
func (p *Pool) Contains(ip netip.Addr) bool {
	if !ip.Is4() {
		return false
	}
	return ip.Compare(p.Start) >= 0 && ip.Compare(p.End) <= 0
}
