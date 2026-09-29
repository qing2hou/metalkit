package dhcp

import (
	"fmt"
	"net/netip"
)

// Range is one inclusive pool segment. A Pool may hold several disjoint
// ranges (e.g. .100-.150 and .180-.200) — allocation scans them in order.
// Mirrors leases.Range; kept separate so the protocol layer stays free of
// store imports (cmd/controller's adapter bridges the two).
type Range struct {
	Start netip.Addr
	End   netip.Addr
}

// PoolRange is the string form used by constructors and SubnetPoolInput.
type PoolRange struct {
	Start string
	End   string
}

// Pool is the parsed/validated DHCP IP pool used in full mode. It is
// constructed from config.DHCPPool / SubnetPoolInput at startup and held by
// reference on Server. Ranges are inclusive; exclude is the set of host IPs
// we must never hand out (e.g. controller's serverIP, the gateway).
//
// Start/End repeat the first range for backwards compatibility with the
// original single-range pool (tests, logging, and the settings path still
// read them).
type Pool struct {
	Start    netip.Addr
	End      netip.Addr
	Netmask  netip.Addr // 255.255.255.0 etc — passed in option 1
	Gateway  netip.Addr // option 3
	DNS      []netip.Addr
	Exclude  map[string]struct{}
	LeaseSec uint32  // option 51
	Ranges   []Range // one or more inclusive segments, in scan order

	// selectors are the relay match keys this pool serves (see pools.go).
	// Empty for the local pool; populated only by NewSubnetPools, never by
	// the plain single-pool path.
	selectors []netip.Prefix
}

// NewPool validates each input and returns a single-range Pool ready for
// the server to hand out IPs from. Inputs are strings so the config layer
// can stay free of net/netip in its YAML schema; the server constructs the
// Pool once.
func NewPool(start, end, netmask, gateway string, dns []string, leaseSec uint32, exclude []string) (*Pool, error) {
	return NewPoolRanges([]PoolRange{{Start: start, End: end}}, netmask, gateway, dns, leaseSec, exclude)
}

// NewPoolRanges builds a multi-range pool. Requires at least one range;
// ranges must be non-overlapping (scan order is preserved, but an overlap
// would double-count the same addresses across "segments").
func NewPoolRanges(ranges []PoolRange, netmask, gateway string, dns []string, leaseSec uint32, exclude []string) (*Pool, error) {
	if len(ranges) == 0 {
		return nil, fmt.Errorf("pool: at least one range required")
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

	parsed := make([]Range, 0, len(ranges))
	for i, r := range ranges {
		startA, err := netip.ParseAddr(r.Start)
		if err != nil || !startA.Is4() {
			return nil, fmt.Errorf("pool range %d start %q: must be IPv4", i, r.Start)
		}
		endA, err := netip.ParseAddr(r.End)
		if err != nil || !endA.Is4() {
			return nil, fmt.Errorf("pool range %d end %q: must be IPv4", i, r.End)
		}
		if startA.Compare(endA) > 0 {
			return nil, fmt.Errorf("pool range %d: start %s > end %s", i, startA, endA)
		}
		for _, prev := range parsed {
			if rangesOverlap(prev, Range{Start: startA, End: endA}) {
				return nil, fmt.Errorf("pool range %d [%s..%s] overlaps [%s..%s]", i, startA, endA, prev.Start, prev.End)
			}
		}
		parsed = append(parsed, Range{Start: startA, End: endA})
	}

	return &Pool{
		Start: parsed[0].Start, End: parsed[0].End,
		Netmask: maskA, Gateway: gwA,
		DNS: dnsA, Exclude: exSet, LeaseSec: leaseSec,
		Ranges: parsed,
	}, nil
}

func rangesOverlap(a, b Range) bool {
	return a.Start.Compare(b.End) <= 0 && b.Start.Compare(a.End) <= 0
}

// Contains reports whether ip is inside any of the pool's ranges. Used to
// validate a client's REQUEST: a REQUEST for an IP outside our pool gets
// NAK'd.
func (p *Pool) Contains(ip netip.Addr) bool {
	if !ip.Is4() {
		return false
	}
	for _, r := range p.Ranges {
		if ip.Compare(r.Start) >= 0 && ip.Compare(r.End) <= 0 {
			return true
		}
	}
	return false
}

// ContainsKey reports whether ip is inside this pool's *network* (first
// range's start subnet under the pool netmask). NewSubnetPools uses it to
// reject relay selectors that could never match, e.g. selector 192.168.1.10
// against a pool leasing 192.168.10.150-200.
func (p *Pool) ContainsKey(ip netip.Addr) bool {
	if !ip.Is4() || !p.Netmask.Is4() {
		return false
	}
	masked := func(a netip.Addr) [4]byte {
		ha, hm := a.As4(), p.Netmask.As4()
		return [4]byte{ha[0] & hm[0], ha[1] & hm[1], ha[2] & hm[2], ha[3] & hm[3]}
	}
	return masked(p.Start) == masked(ip)
}

// RangesCompact renders the ranges for logs, e.g. "192.168.1.100-192.168.1.150,192.168.1.180-192.168.1.200".
func (p *Pool) RangesCompact() string {
	out := ""
	for i, r := range p.Ranges {
		if i > 0 {
			out += ","
		}
		out += r.Start.String() + "-" + r.End.String()
	}
	return out
}
