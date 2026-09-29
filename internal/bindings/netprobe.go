package bindings

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// IPProber answers "does this IPv4 already belong to some device on the
// local L2 segment?". Allocation uses it to avoid handing out an address
// that is live but invisible to our catalogs (a router, a printer, a host
// configured by hand). Implementations must be safe for concurrent use.
type IPProber interface {
	// InUseBatch probes every ip and returns the subset that answered.
	// The returned map keys are canonical dotted-quad strings.
	InUseBatch(ctx context.Context, ips []net.IP) (map[string]bool, error)
}

// ARPProber is the production IPProber: it emits ARP requests (RFC 826) on
// the interface whose subnet covers the candidate and collects ARP replies
// for a short window. ARP is used rather than ICMP because it needs no
// cooperation from the target's firewall and works on any host in the
// segment. Requires CAP_NET_RAW (the controller runs as root).
type ARPProber struct {
	// Timeout is the per-batch receive window. Zero means 750ms, which is
	// comfortably above the round-trip on a switched LAN.
	Timeout time.Duration
}

// arpBatchSize caps how many candidates share one probe window; larger
// batches risk dropping replies from a busy exchange.
const arpBatchSize = 16

func (p *ARPProber) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return 750 * time.Millisecond
}

// InUseBatch implements IPProber. Candidates are grouped by the local
// interface that covers them and probed in windows of arpBatchSize. If no
// local interface covers a candidate (e.g. a VLAN the controller is not on),
// it is reported via the error so the caller can decide to skip probing —
// the map returned alongside still holds whatever was learned.
func (p *ARPProber) InUseBatch(ctx context.Context, ips []net.IP) (map[string]bool, error) {
	out := make(map[string]bool)
	if len(ips) == 0 {
		return out, nil
	}

	type group struct {
		ifc *net.Interface
		src net.IP
		ips []net.IP
	}
	groups := map[int]*group{}
	var uncovered []string
	for _, ip := range ips {
		ip4 := ip.To4()
		if ip4 == nil {
			continue
		}
		ifc, src, err := ifaceForIP(ip4)
		if err != nil {
			uncovered = append(uncovered, ip4.String())
			continue
		}
		g, ok := groups[ifc.Index]
		if !ok {
			g = &group{ifc: ifc, src: src}
			groups[ifc.Index] = g
		}
		g.ips = append(g.ips, ip4)
	}

	for _, g := range groups {
		for i := 0; i < len(g.ips); i += arpBatchSize {
			end := i + arpBatchSize
			if end > len(g.ips) {
				end = len(g.ips)
			}
			if err := probeBatchCtx(ctx, g.ifc, g.src, g.ips[i:end], p.timeout(), out); err != nil {
				return out, err
			}
		}
	}

	if len(uncovered) > 0 && len(groups) == 0 {
		return out, fmt.Errorf("no local interface covers %v", uncovered)
	}
	return out, nil
}

// probeBatchCtx sends ARP requests for ips from ifc and records replies.
func probeBatchCtx(ctx context.Context, ifc *net.Interface, src net.IP, ips []net.IP, window time.Duration, out map[string]bool) error {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ARP)))
	if err != nil {
		return fmt.Errorf("arp socket: %w", err)
	}
	defer unix.Close(fd)

	if err := unix.Bind(fd, &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ARP),
		Ifindex:  ifc.Index,
	}); err != nil {
		return fmt.Errorf("arp bind %s: %w", ifc.Name, err)
	}

	dst := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ARP),
		Ifindex:  ifc.Index,
		Halen:    6,
		Addr:     [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	}
	// Two request rounds: a single lost broadcast (busy target, congested
	// segment) would otherwise read as "free" and hand out a live address.
	sendRound := func() error {
		for _, ip := range ips {
			if err := ctx.Err(); err != nil {
				return err
			}
			frame := buildARPRequest(ifc.HardwareAddr, src, ip)
			if err := unix.Sendto(fd, frame, 0, dst); err != nil {
				return fmt.Errorf("arp send on %s: %w", ifc.Name, err)
			}
		}
		return nil
	}
	if err := sendRound(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(120 * time.Millisecond):
	}
	if err := sendRound(); err != nil {
		return err
	}

	deadline := time.Now().Add(window)
	buf := make([]byte, 512)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// SO_RCVTIMEO bounds each read; EAGAIN means the window elapsed.
		tv := unix.NsecToTimeval(remaining.Nanoseconds())
		if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
			return fmt.Errorf("arp setsockopt: %w", err)
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				return nil
			}
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("arp recv: %w", err)
		}
		if ip, ok := parseARPReply(buf[:n]); ok {
			out[ip] = true
			if debugARP {
				fmt.Printf("[arp] reply from %s (%d bytes)\n", ip, n)
			}
		} else if debugARP {
			fmt.Printf("[arp] frame ignored: len=%d op=%d ethertype=0x%04x\n",
				n, frameOP(buf[:n]), frameEtherType(buf[:n]))
		}
	}
}

// buildARPRequest builds a broadcast ARP request ("who has dst?") claiming src
// as the sender address on mac.
func buildARPRequest(mac net.HardwareAddr, src, dst net.IP) []byte {
	frame := make([]byte, 42)
	copy(frame[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) // dst: broadcast
	copy(frame[6:12], mac)                                       // src mac
	binary.BigEndian.PutUint16(frame[12:14], unix.ETH_P_ARP)
	binary.BigEndian.PutUint16(frame[14:16], 1)      // htype: ethernet
	binary.BigEndian.PutUint16(frame[16:18], 0x0800) // ptype: IPv4
	frame[18] = 6                                    // hlen
	frame[19] = 4                                    // plen
	binary.BigEndian.PutUint16(frame[20:22], 1)      // op: request
	copy(frame[22:28], mac)                          // sender mac
	copy(frame[28:32], src.To4())                    // sender IP
	// target mac stays zeroed (unknown)
	copy(frame[38:42], dst.To4()) // target IP
	return frame
}

// parseARPReply extracts the sender IP from an ARP reply frame, or reports
// false for anything else (our own requests, other protocols, truncated).
func parseARPReply(b []byte) (string, bool) {
	if len(b) < 42 {
		return "", false
	}
	if binary.BigEndian.Uint16(b[12:14]) != unix.ETH_P_ARP {
		return "", false
	}
	if binary.BigEndian.Uint16(b[20:22]) != 2 { // op: reply
		return "", false
	}
	return net.IPv4(b[28], b[29], b[30], b[31]).String(), true
}

// ifaceForIP finds an up, non-loopback interface whose IPv4 subnet contains
// ip, returning the interface and that interface's source address.
func ifaceForIP(ip net.IP) (*net.Interface, net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, fmt.Errorf("list interfaces: %w", err)
	}
	for i := range ifaces {
		ifc := &ifaces[i]
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(ifc.HardwareAddr) < 6 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			src := n.IP.To4()
			if src == nil {
				continue
			}
			if n.Contains(ip) {
				return ifc, src, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("no local interface covers %s", ip)
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// debugARP enables frame-level logging when METALKIT_ARP_DEBUG is set.
var debugARP = os.Getenv("METALKIT_ARP_DEBUG") != ""

func frameEtherType(b []byte) int {
	if len(b) < 14 {
		return -1
	}
	return int(binary.BigEndian.Uint16(b[12:14]))
}

func frameOP(b []byte) int {
	if len(b) < 22 {
		return -1
	}
	return int(binary.BigEndian.Uint16(b[20:22]))
}
