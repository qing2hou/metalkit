package bindings

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// stubProber is a scripted IPProber for composite-logic tests.
type stubProber struct {
	live  map[string]bool
	err   error
	calls [][]string
}

func (s *stubProber) InUseBatch(_ context.Context, ips []net.IP) (map[string]bool, error) {
	batch := make([]string, 0, len(ips))
	for _, ip := range ips {
		batch = append(batch, ip.String())
	}
	s.calls = append(s.calls, batch)
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]bool{}
	for _, ip := range ips {
		if s.live[ip.String()] {
			out[ip.String()] = true
		}
	}
	return out, nil
}

func ipsOf(addrs ...string) []net.IP {
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, net.ParseIP(a))
	}
	return out
}

// TestCompositeARPIsolatedSegment: ARP is silenced by port isolation, but the
// ICMP cross-check still finds the live host — the address must be reported
// in use rather than handed out.
func TestCompositeARPIsolatedSegment(t *testing.T) {
	arp := &stubProber{live: map[string]bool{}} // isolation: nothing answers
	icmp := &stubProber{live: map[string]bool{"10.0.0.7": true}}
	c := &CompositeProber{ARP: arp, ICMP: icmp}

	got, err := c.InUseBatch(context.Background(), ipsOf("10.0.0.7", "10.0.0.8"))
	if err != nil {
		t.Fatalf("InUseBatch: %v", err)
	}
	if !got["10.0.0.7"] {
		t.Error("10.0.0.7 must be reported live (ICMP caught what ARP isolation hid)")
	}
	if got["10.0.0.8"] {
		t.Error("10.0.0.8 should stay free")
	}
	// ICMP must only be asked about the silent candidates.
	if len(icmp.calls) != 1 || len(icmp.calls[0]) != 2 {
		t.Fatalf("icmp calls = %v, want one batch of the 2 silent candidates", icmp.calls)
	}
}

// TestCompositeNoL2UsesICMP: no local interface covers the subnet (ARP errors
// out) — the ICMP verdict stands alone.
func TestCompositeNoL2UsesICMP(t *testing.T) {
	arp := &stubProber{err: errors.New("no local interface covers 172.16.31.2")}
	icmp := &stubProber{live: map[string]bool{"172.16.31.9": true}}
	c := &CompositeProber{ARP: arp, ICMP: icmp}

	got, err := c.InUseBatch(context.Background(), ipsOf("172.16.31.9", "172.16.31.10"))
	if err != nil {
		t.Fatalf("InUseBatch: %v", err)
	}
	if !got["172.16.31.9"] {
		t.Error("routed ICMP verdict must be used when ARP is unavailable")
	}
	if len(icmp.calls) != 1 || len(icmp.calls[0]) != 2 {
		t.Fatalf("icmp calls = %v, want the full batch (ARP gave nothing)", icmp.calls)
	}
}

// TestCompositeBothUnavailable: neither probe can run — the error must reach
// the allocator so it can say so in its log instead of pretending to know.
func TestCompositeBothUnavailable(t *testing.T) {
	arp := &stubProber{err: errors.New("no local interface covers 172.16.31.2")}
	icmp := &stubProber{err: errors.New("icmp socket: operation not permitted")}
	c := &CompositeProber{ARP: arp, ICMP: icmp}

	_, err := c.InUseBatch(context.Background(), ipsOf("172.16.31.9"))
	if err == nil {
		t.Fatal("expected an error when both probes are unusable")
	}
	if !strings.Contains(err.Error(), "icmp") || !strings.Contains(err.Error(), "arp") {
		t.Errorf("error should name both layers, got: %v", err)
	}
}

// TestCompositeICMPLossKeepsARPVerdict: on a healthy L2 segment an unusable
// ICMP cross-check must not discard ARP's answer.
func TestCompositeICMPLossKeepsARPVerdict(t *testing.T) {
	arp := &stubProber{live: map[string]bool{"10.0.0.5": true}}
	icmp := &stubProber{err: errors.New("icmp socket: operation not permitted")}
	c := &CompositeProber{ARP: arp, ICMP: icmp}

	got, err := c.InUseBatch(context.Background(), ipsOf("10.0.0.5", "10.0.0.6"))
	if err != nil {
		t.Fatalf("ICMP degradation must not fail the probe: %v", err)
	}
	if !got["10.0.0.5"] {
		t.Error("ARP's live verdict must survive an ICMP failure")
	}
	if got["10.0.0.6"] {
		t.Error("10.0.0.6 has no live verdict from either layer")
	}
}

// TestCompositeARPLiveSkipsICMP: nothing silent → no ICMP traffic at all.
func TestCompositeARPLiveSkipsICMP(t *testing.T) {
	arp := &stubProber{live: map[string]bool{"10.0.0.5": true}}
	icmp := &stubProber{}
	c := &CompositeProber{ARP: arp, ICMP: icmp}

	if _, err := c.InUseBatch(context.Background(), ipsOf("10.0.0.5")); err != nil {
		t.Fatalf("InUseBatch: %v", err)
	}
	if len(icmp.calls) != 0 {
		t.Errorf("icmp should not be called when ARP found everything live: %v", icmp.calls)
	}
}
