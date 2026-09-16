package bindings

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// ICMPProber is the routed-network liveness prober: it sends ICMP echo
// requests and watches for replies. Unlike ARP it needs no L2 adjacency, so
// it still works for subnets the controller only reaches through a router —
// and it survives ARP filtering/port isolation on the target segment, where
// broadcast ARP never reaches the peers (see CompositeProber).
//
// Trade-off: a host that blocks ICMP looks free. That is why ICMP results are
// layered on top of ARP rather than replacing it, and why "silent" is treated
// as "not known to be live" rather than proof of availability.
type ICMPProber struct {
	// Timeout is the reply window per batch; zero means 1s.
	Timeout time.Duration
}

// probePayload marks our echo requests so replies can be matched even on
// unprivileged sockets (where the kernel rewrites the ICMP id).
var probePayload = []byte("metalkit-probe")

func (p *ICMPProber) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return time.Second
}

// InUseBatch implements IPProber. Errors mean "ICMP probing is not usable"
// (no privileges, no route) — never "everything is free".
func (p *ICMPProber) InUseBatch(ctx context.Context, ips []net.IP) (map[string]bool, error) {
	out := make(map[string]bool)
	if len(ips) == 0 {
		return out, nil
	}

	// Prefer the raw socket (root: full control over the ICMP id). Fall back
	// to the unprivileged "udp4" mapping when the kernel allows it
	// (net.ipv4.ping_group_range) — in that mode the id is rewritten, so
	// replies are matched on the payload we sent instead.
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	privileged := err == nil
	if err != nil {
		conn, err = icmp.ListenPacket("udp4", "0.0.0.0")
		if err != nil {
			return nil, fmt.Errorf("icmp socket (ip4:icmp and udp4 both failed): %w", err)
		}
	}
	defer conn.Close()

	id := os.Getpid() & 0xffff
	probing := make(map[string]bool, len(ips))
	seq := 0
	for _, ip := range ips {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		ip4 := ip.To4()
		if ip4 == nil {
			continue
		}
		seq++
		msg := icmp.Message{
			Type: ipv4.ICMPTypeEcho,
			Code: 0,
			Body: &icmp.Echo{ID: id, Seq: seq, Data: probePayload},
		}
		wire, err := msg.Marshal(nil)
		if err != nil {
			return out, fmt.Errorf("icmp marshal: %w", err)
		}
		dst := net.Addr(&net.IPAddr{IP: ip4})
		if !privileged {
			dst = &net.UDPAddr{IP: ip4}
		}
		if _, err := conn.WriteTo(wire, dst); err != nil {
			// A per-target send failure (no route, net unreachable) means the
			// probe cannot judge this subnet — report it instead of guessing.
			return out, fmt.Errorf("icmp send to %s: %w", ip4, err)
		}
		probing[ip4.String()] = true
	}
	if len(probing) == 0 {
		return out, nil
	}

	deadline := time.Now().Add(p.timeout())
	buf := make([]byte, 1500)
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return out, nil
		}
		if err := conn.SetReadDeadline(time.Now().Add(remaining)); err != nil {
			return out, fmt.Errorf("icmp deadline: %w", err)
		}
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return out, nil
			}
			return out, fmt.Errorf("icmp recv: %w", err)
		}
		if peerIP := addrIP(peer); peerIP != "" {
			if !probing[peerIP] {
				continue // someone else's traffic / not a candidate
			}
			rm, err := icmp.ParseMessage(1, buf[:n]) // 1 = ICMPv4
			if err != nil {
				continue
			}
			if rm.Type != ipv4.ICMPTypeEchoReply {
				// Dest-unreachable and friends mean "no host there"; keep waiting.
				continue
			}
			echo, ok := rm.Body.(*icmp.Echo)
			if !ok {
				continue
			}
			if privileged && echo.ID != id {
				continue
			}
			if string(echo.Data) != string(probePayload) {
				continue
			}
			out[peerIP] = true
		}
	}
}

// addrIP extracts the IPv4 string from an ICMP reply's source address, which
// is *net.IPAddr for raw sockets and *net.UDPAddr for the unprivileged path.
func addrIP(a net.Addr) string {
	switch v := a.(type) {
	case *net.IPAddr:
		if ip4 := v.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	case *net.UDPAddr:
		if ip4 := v.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return ""
}

// CompositeProber layers the two probes so each covers the other's blind
// spot:
//
//   - ARP works only on the local L2 segment; ICMP also crosses routers.
//   - ARP can be silenced wholesale by switch port isolation / ARP filtering
//     (broadcast never reaches the peers) which would read as "free"; a
//     unicast ICMP echo often still gets through, so silent ARP candidates
//     are re-checked with ICMP.
//
// Errors from either layer are only surfaced when a verdict cannot be formed
// at all; when ARP produced one, an ICMP failure degrades the confidence
// without invalidating the answer (logged, not fatal).
type CompositeProber struct {
	ARP    IPProber
	ICMP   IPProber
	Logger loggerLike // optional; nil silences degradation notes
}

// loggerLike is the slice of *slog.Logger the composite needs, kept narrow so
// tests can pass a stub without pulling slog into their assertions.
type loggerLike interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// InUseBatch implements IPProber.
func (c *CompositeProber) InUseBatch(ctx context.Context, ips []net.IP) (map[string]bool, error) {
	switch {
	case c.ARP == nil && c.ICMP == nil:
		return nil, fmt.Errorf("no prober configured")
	case c.ARP == nil:
		return c.ICMP.InUseBatch(ctx, ips)
	case c.ICMP == nil:
		return c.ARP.InUseBatch(ctx, ips)
	}

	live, arpErr := c.ARP.InUseBatch(ctx, ips)
	if arpErr != nil {
		// Controller is not attached to this subnet's L2 (a VLAN it only
		// reaches by routing). ICMP is the only probe left.
		icmpLive, icmpErr := c.ICMP.InUseBatch(ctx, ips)
		if icmpErr != nil {
			return live, fmt.Errorf("arp: %v; icmp: %w", arpErr, icmpErr)
		}
		if c.Logger != nil {
			c.Logger.Info("liveness probe: ARP unavailable, ICMP verdict used",
				"arp_err", arpErr.Error())
		}
		return icmpLive, nil
	}

	// ARP covered the segment. Re-verify its silent candidates with ICMP:
	// on ARP-isolated segments every host looks silent, and assigning one of
	// those addresses would collide.
	var silent []net.IP
	for _, ip := range ips {
		if !live[ip.String()] {
			silent = append(silent, ip)
		}
	}
	if len(silent) == 0 {
		return live, nil
	}
	icmpLive, icmpErr := c.ICMP.InUseBatch(ctx, silent)
	if icmpErr != nil {
		// Keep the ARP verdict (authoritative for L2) but make the reduced
		// confidence visible.
		if c.Logger != nil {
			c.Logger.Warn("liveness probe: ICMP cross-check unavailable; ARP verdict kept",
				"err", icmpErr.Error())
		}
		return live, nil
	}
	for ip := range icmpLive {
		live[ip] = true
	}
	return live, nil
}
