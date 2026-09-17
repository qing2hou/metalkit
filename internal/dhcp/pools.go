package dhcp

import (
	"fmt"
	"net/netip"
)

// SubnetPools routes DHCP packets to per-subnet pools. Full mode used to
// hold a single global *Pool, which is correct only for clients on the
// controller's own L2 segment; a DHCP relay (e.g. H3C `dhcp select relay`)
// forwards DISCOVERs from other VLANs with giaddr set to the relay's SVI
// address, and those clients must be leased from *their* subnet's pool —
// handing 192.168.10.x to a 192.168.1.x client leaves it with a gateway
// that does not exist on its VLAN and PXE dies before TFTP.
//
// Selection rule (RFC 2131 §4.3.2 in spirit — the server picks the pool
// whose subnet contains the "gateway address" the packet arrived through):
//
//   - giaddr set   → the first relay pool whose selector contains giaddr.
//   - giaddr empty → the local pool (clients on our own segment).
//
// Construction always yields a valid set: exactly one local pool (also the
// fallback for a direct client whose packet carries no giaddr, preserving
// the single-pool behaviour of older deployments), plus zero or more relay
// pools keyed by an IP or CIDR selector. Duplicate selectors are rejected
// so matching stays deterministic. A set giaddr that matches no selector
// returns no pool at all — the server stays silent — rather than
// mis-leasing from the local subnet.
// SubnetPools is immutable after construction: the Server swaps the whole
// pointer under its own lock on reload (same as the old single-pool
// design), so no internal locking is needed.
type SubnetPools struct {
	local *Pool   // direct/L2 clients; never nil
	relay []*Pool // relayed clients, matched in order
}

// SubnetPoolInput describes one pool to load into a SubnetPools. Selector is
// the match key for relayed packets: a plain IPv4 address (the relay SVI,
// e.g. "192.168.1.10") or an IPv4 CIDR covering every possible SVI on that
// segment ("192.168.1.1/24"). Empty Selector means this is the local pool.
// Pools lists one or more address ranges; the legacy single-range fields
// (Start/End) are folded into Pools when Pools is empty.
type SubnetPoolInput struct {
	Selector string // "" | IPv4 | IPv4/prefix
	Pools    []PoolRange
	// Legacy single-range fields, used when Pools is empty (the local pool
	// from settings carries exactly one range).
	Start    string
	End      string
	Netmask  string
	Gateway  string
	DNS      []string
	LeaseSec uint32
	Exclude  []string
}

// NewSubnetPools validates and assembles a pool set. Applies the same
// per-pool validation as NewPoolRanges plus the cross-pool rules above.
// Exactly one input must carry an empty Selector.
func NewSubnetPools(in []SubnetPoolInput) (*SubnetPools, error) {
	var local *Pool
	var relay []*Pool
	var sels []netip.Prefix
	for _, i := range in {
		ranges := i.Pools
		if len(ranges) == 0 {
			ranges = []PoolRange{{Start: i.Start, End: i.End}}
		}
		pool, err := NewPoolRanges(ranges, i.Netmask, i.Gateway, i.DNS, i.LeaseSec, i.Exclude)
		if err != nil {
			return nil, fmt.Errorf("selector %q: %w", i.Selector, err)
		}
		if i.Selector == "" {
			if local != nil {
				return nil, fmt.Errorf("multiple local pools: only one input may omit Selector")
			}
			local = pool
			continue
		}
		sel, ok := tryParseSelector(i.Selector)
		if !ok {
			return nil, fmt.Errorf("selector %q: must be an IPv4 address or IPv4 CIDR", i.Selector)
		}
		// Reject overlaps, not just exact duplicates: "192.168.1.0/24" and
		// "192.168.1.10" differ as strings but both match the same relay,
		// and first-match-wins routing would silently pick an arbitrary one.
		for _, prev := range sels {
			if prev.Overlaps(sel) {
				return nil, fmt.Errorf("selector %q: overlaps existing selector %s", i.Selector, prev)
			}
		}
		sels = append(sels, sel)
		// The pool must contain its selection key, else the route can never
		// fire and the relayed VLAN silently falls back to the local pool —
		// the original bug wearing a different hat.
		if !pool.ContainsKey(sel.Addr()) {
			return nil, fmt.Errorf("selector %q is outside the pool network %s: the relay SVI address must fall inside the pool's subnet", i.Selector, pool.RangesCompact())
		}
		pool.selectors = append(pool.selectors, sel)
		relay = append(relay, pool)
	}
	if local == nil {
		return nil, fmt.Errorf("no local pool: exactly one input must omit Selector")
	}
	return &SubnetPools{local: local, relay: relay}, nil
}

// tryParseSelector accepts "a.b.c.d" (as a /32 match key) and "a.b.c.d/p",
// returning the masked prefix.
func tryParseSelector(s string) (netip.Prefix, bool) {
	if p, err := netip.ParsePrefix(s); err == nil {
		if !p.Addr().Is4() {
			return netip.Prefix{}, false
		}
		return p.Masked(), true
	}
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, 32), true
}

// poolForRequest resolves the pool serving an incoming packet. giaddr is
// the packet's gateway address (0.0.0.0 for direct clients). Returns nil
// when a relayed giaddr matches no selector — the caller should not answer.
func (sp *SubnetPools) poolForRequest(giaddr netip.Addr) *Pool {
	if sp == nil {
		return nil
	}
	if !giaddr.IsValid() || giaddr.IsUnspecified() || !giaddr.Is4() {
		return sp.local
	}
	for _, p := range sp.relay {
		for _, sel := range p.selectors {
			if sel.Contains(giaddr) {
				return p
			}
		}
	}
	return nil
}

// Local returns the direct-client pool (never nil on a valid set).
func (sp *SubnetPools) Local() *Pool {
	if sp == nil {
		return nil
	}
	return sp.local
}

// RelayPools returns the relay pools (for logging/counting).
func (sp *SubnetPools) RelayPools() []*Pool {
	if sp == nil {
		return nil
	}
	return sp.relay
}

// relaySelectors flattens the relay pools' selectors for startup logging.
func (sp *SubnetPools) RelaySummary() []string {
	if sp == nil {
		return nil
	}
	out := make([]string, 0, len(sp.relay))
	for _, p := range sp.relay {
		sels := make([]string, 0, len(p.selectors))
		for _, s := range p.selectors {
			sels = append(sels, s.String())
		}
		out = append(out, fmt.Sprintf("%s[%s]", p.RangesCompact(), joinComma(sels)))
	}
	return out
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
