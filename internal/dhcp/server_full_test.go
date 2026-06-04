package dhcp

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

// fakeLeaseStore is an in-memory LeaseStore that records calls so tests can
// assert what the protocol layer asked for without dragging in SQLite.
type fakeLeaseStore struct {
	allocIP      string
	allocErr     error
	confirmIP    string
	confirmErr   error
	releaseCalls []string
	confirmCalls []confirmCall
}

type confirmCall struct {
	mac      string
	reqIP    string
	leaseDur time.Duration
}

func (f *fakeLeaseStore) Allocate(_ context.Context, _ AllocateInput) (string, error) {
	if f.allocErr != nil {
		return "", f.allocErr
	}
	if f.allocIP == "" {
		return "192.168.10.100", nil
	}
	return f.allocIP, nil
}

func (f *fakeLeaseStore) Confirm(_ context.Context, mac, ip string, d time.Duration) (string, error) {
	f.confirmCalls = append(f.confirmCalls, confirmCall{mac, ip, d})
	if f.confirmErr != nil {
		return "", f.confirmErr
	}
	if f.confirmIP != "" {
		return f.confirmIP, nil
	}
	return ip, nil
}

func (f *fakeLeaseStore) Release(_ context.Context, mac string) error {
	f.releaseCalls = append(f.releaseCalls, mac)
	return nil
}

func fullCfg(t *testing.T, fake *fakeLeaseStore) *Config {
	t.Helper()
	pool, err := NewPool("192.168.10.100", "192.168.10.200",
		"255.255.255.0", "192.168.10.1",
		[]string{"8.8.8.8"}, 3600,
		[]string{"192.168.10.120"},
	)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return &Config{
		Interface:  "lo",
		ListenAddr: ":67",
		ServerIP:   "192.168.10.120",
		HTTPURL:    "http://192.168.10.120:8080/boot/ipxe",
		Mode:       ModeFull,
		Pool:       pool,
		Leases:     fake,
	}
}

func newServerFull(t *testing.T, fake *fakeLeaseStore) *Server {
	t.Helper()
	srv, err := New(*fullCfg(t, fake))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestFullMode_DiscoverNonPXE_GetsOffer(t *testing.T) {
	// A plain DHCP client (no PXEClient class) must still get an OFFER in
	// full mode — this is what makes metalkit "the LAN's DHCP server".
	fake := &fakeLeaseStore{}
	srv := newServerFull(t, fake)
	req := mustReq(t, dhcpv4.MessageTypeDiscover)
	reply, err := srv.buildFullReply(context.Background(), req)
	if err != nil {
		t.Fatalf("buildFullReply: %v", err)
	}
	if reply == nil {
		t.Fatal("expected reply, got nil")
	}
	if reply.MessageType() != dhcpv4.MessageTypeOffer {
		t.Errorf("MessageType = %v, want Offer", reply.MessageType())
	}
	if got := reply.YourIPAddr.String(); got != "192.168.10.100" {
		t.Errorf("yiaddr = %q, want 192.168.10.100", got)
	}
	// Standard lease options must be present.
	if mask := reply.SubnetMask(); mask == nil {
		t.Error("option 1 (subnet mask) missing")
	}
	if r := reply.Router(); len(r) == 0 || !r[0].Equal(net.IPv4(192, 168, 10, 1)) {
		t.Errorf("option 3 (router) = %v, want 192.168.10.1", r)
	}
	if d := reply.DNS(); len(d) == 0 || !d[0].Equal(net.IPv4(8, 8, 8, 8)) {
		t.Errorf("option 6 (dns) = %v, want 8.8.8.8", d)
	}
	if reply.BootFileName != "" {
		t.Errorf("non-PXE OFFER must not set bootfile, got %q", reply.BootFileName)
	}
}

func TestFullMode_DiscoverPXE_GetsBootfile(t *testing.T) {
	// PXE first-stage DISCOVER in full mode: OFFER carries BOTH the IP
	// lease AND the bootfile (we're the only DHCP server, no conflict).
	fake := &fakeLeaseStore{}
	srv := newServerFull(t, fake)
	req := mustReq(t, dhcpv4.MessageTypeDiscover,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00007:UNDI:003016")),
		dhcpv4.WithOption(dhcpv4.OptClientArch(iana.Arch(0x0007))),
	)
	reply, err := srv.buildFullReply(context.Background(), req)
	if err != nil {
		t.Fatalf("buildFullReply: %v", err)
	}
	if reply == nil {
		t.Fatal("expected reply")
	}
	if reply.BootFileName != "snponly.efi" {
		t.Errorf("BootFileName = %q, want snponly.efi", reply.BootFileName)
	}
	if reply.YourIPAddr.IsUnspecified() {
		t.Error("yiaddr unset — PXE client still needs an IP")
	}
}

func TestFullMode_RequestInPool_GetsAck(t *testing.T) {
	// A REQUEST for an IP we previously offered (inside the pool) gets ACK
	// and calls Confirm on the store.
	fake := &fakeLeaseStore{}
	srv := newServerFull(t, fake)
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.IPv4(192, 168, 10, 100))),
	)
	reply, err := srv.buildFullReply(context.Background(), req)
	if err != nil {
		t.Fatalf("buildFullReply: %v", err)
	}
	if reply == nil || reply.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatalf("want ACK, got %v", reply)
	}
	if len(fake.confirmCalls) != 1 {
		t.Fatalf("Confirm not called, got %d", len(fake.confirmCalls))
	}
	if fake.confirmCalls[0].reqIP != "192.168.10.100" {
		t.Errorf("requested IP = %q", fake.confirmCalls[0].reqIP)
	}
}

func TestFullMode_RequestOutOfPool_GetsNak(t *testing.T) {
	// A REQUEST for an IP outside our pool must produce NAK so the client
	// restarts DISCOVER immediately rather than timing out.
	fake := &fakeLeaseStore{}
	srv := newServerFull(t, fake)
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.IPv4(10, 0, 0, 5))),
	)
	reply, err := srv.buildFullReply(context.Background(), req)
	if err != nil {
		t.Fatalf("buildFullReply: %v", err)
	}
	if reply == nil || reply.MessageType() != dhcpv4.MessageTypeNak {
		t.Fatalf("want NAK, got %v", reply)
	}
	if len(fake.confirmCalls) != 0 {
		t.Error("Confirm should not be called on out-of-pool REQUEST")
	}
}

func TestFullMode_RequestConfirmFails_GetsNak(t *testing.T) {
	// If the store rejects Confirm (e.g. requested IP doesn't match the
	// offered one) we NAK.
	fake := &fakeLeaseStore{confirmErr: errors.New("mismatch")}
	srv := newServerFull(t, fake)
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.IPv4(192, 168, 10, 100))),
	)
	reply, err := srv.buildFullReply(context.Background(), req)
	if err != nil {
		t.Fatalf("buildFullReply: %v", err)
	}
	if reply == nil || reply.MessageType() != dhcpv4.MessageTypeNak {
		t.Fatalf("want NAK, got %v", reply)
	}
}

func TestFullMode_RequestNoOption50_UsesStoreIPForYiaddr(t *testing.T) {
	// iPXE's second-stage REQUEST sometimes omits option 50 AND ciaddr — the
	// server must look up the lease for this MAC and fill yiaddr from it,
	// or iPXE bails with "No configuration methods succeeded" and the whole
	// PXE chain dies before the live image ever boots. Regression guard for
	// the "无法纳管" symptom reported against 192.168.10.120.
	fake := &fakeLeaseStore{confirmIP: "192.168.10.90"}
	srv := newServerFull(t, fake)
	req := mustReq(t, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00007:UNDI:003016")),
		dhcpv4.WithOption(dhcpv4.OptUserClass("iPXE")),
	)
	reply, err := srv.buildFullReply(context.Background(), req)
	if err != nil {
		t.Fatalf("buildFullReply: %v", err)
	}
	if reply == nil || reply.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatalf("want ACK, got %v", reply)
	}
	if got := reply.YourIPAddr.String(); got != "192.168.10.90" {
		t.Errorf("yiaddr = %q, want 192.168.10.90 (from store)", got)
	}
}

func TestFullMode_ReleaseCallsStore(t *testing.T) {
	// RELEASE / DECLINE both forward to the store. No reply is sent.
	fake := &fakeLeaseStore{}
	srv := newServerFull(t, fake)
	for _, mt := range []dhcpv4.MessageType{dhcpv4.MessageTypeRelease, dhcpv4.MessageTypeDecline} {
		req := mustReq(t, mt)
		reply, err := srv.buildFullReply(context.Background(), req)
		if err != nil {
			t.Fatalf("%v: %v", mt, err)
		}
		if reply != nil {
			t.Errorf("%v: expected nil reply (no answer), got %v", mt, reply.Summary())
		}
	}
	if len(fake.releaseCalls) != 2 {
		t.Errorf("Release calls = %d, want 2", len(fake.releaseCalls))
	}
}

func TestPool_Contains(t *testing.T) {
	pool, err := NewPool("10.0.0.10", "10.0.0.20", "255.255.255.0",
		"10.0.0.1", []string{"1.1.1.1"}, 3600, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	cases := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.9", false},
		{"10.0.0.10", true},
		{"10.0.0.15", true},
		{"10.0.0.20", true},
		{"10.0.0.21", false},
	}
	for _, c := range cases {
		got := pool.Contains(netip.MustParseAddr(c.ip))
		if got != c.want {
			t.Errorf("Contains(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestPool_RejectsInvalidInput(t *testing.T) {
	cases := []struct{ name, start, end, mask, gw string }{
		{"bad start", "not-an-ip", "10.0.0.20", "255.255.255.0", "10.0.0.1"},
		{"bad end", "10.0.0.10", "x", "255.255.255.0", "10.0.0.1"},
		{"start > end", "10.0.0.20", "10.0.0.10", "255.255.255.0", "10.0.0.1"},
		{"bad mask", "10.0.0.10", "10.0.0.20", "nope", "10.0.0.1"},
		{"bad gw", "10.0.0.10", "10.0.0.20", "255.255.255.0", "nope"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewPool(c.start, c.end, c.mask, c.gw, nil, 0, nil)
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestReload_SwapsPoolAtomically(t *testing.T) {
	// After Reload with a new pool, a fresh DISCOVER must allocate from the
	// new range — this is the core of the hot-reload UX (operator saves new
	// settings in the UI; clients picked up the next DHCP packet see the
	// new pool with no restart).
	fake := &fakeLeaseStore{allocIP: "10.0.0.5"}
	srv := newServerFull(t, fake)

	newPool, err := NewPool("10.0.0.1", "10.0.0.50", "255.255.255.0",
		"10.0.0.254", []string{"1.1.1.1"}, 7200, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if err := srv.Reload(ModeFull, newPool, fake); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	req := mustReq(t, dhcpv4.MessageTypeDiscover)
	reply, err := srv.buildFullReply(context.Background(), req)
	if err != nil {
		t.Fatalf("buildFullReply: %v", err)
	}
	if reply == nil || reply.YourIPAddr.String() != "10.0.0.5" {
		t.Errorf("yiaddr = %v, want 10.0.0.5 (from new pool)", reply.YourIPAddr)
	}
	if r := reply.Router(); len(r) == 0 || r[0].String() != "10.0.0.254" {
		t.Errorf("router = %v, want 10.0.0.254 (from new pool)", r)
	}
}

func TestReload_RejectsInvalidConfig(t *testing.T) {
	// Reload to full without a Pool/Leases store must return an error and
	// leave the server's old state intact — a half-applied swap would make
	// the next OFFER nil-deref.
	fake := &fakeLeaseStore{}
	srv := newServerFull(t, fake)

	if err := srv.Reload(ModeFull, nil, fake); err == nil {
		t.Error("expected error for full mode without Pool")
	}
	if err := srv.Reload(ModeFull, srv.pool, nil); err == nil {
		t.Error("expected error for full mode without Leases")
	}
	// Old state must still work after the rejected reloads.
	req := mustReq(t, dhcpv4.MessageTypeDiscover)
	reply, err := srv.buildFullReply(context.Background(), req)
	if err != nil || reply == nil {
		t.Fatalf("post-failed-reload DISCOVER broken: err=%v reply=%v", err, reply)
	}
}

