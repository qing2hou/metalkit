package bindings

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeProber is a scripted IPProber: live holds the addresses that "answer".
// When err is set every call fails, exercising the unprobed fallback.
type fakeProber struct {
	live   map[string]bool
	err    error
	probes [][]string // batches as seen, for ordering assertions
}

func (f *fakeProber) InUseBatch(_ context.Context, ips []net.IP) (map[string]bool, error) {
	batch := make([]string, 0, len(ips))
	for _, ip := range ips {
		batch = append(batch, ip.String())
	}
	f.probes = append(f.probes, batch)
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]bool{}
	for _, ip := range ips {
		if f.live[ip.String()] {
			out[ip.String()] = true
		}
	}
	return out, nil
}

const allocSubnet = "10.9.0.0/24"

// TestAllocateSkipsLiveAddresses: candidates that answer the liveness probe
// must be skipped; the first silent address wins.
func TestAllocateSkipsLiveAddresses(t *testing.T) {
	f := newFixture(t)
	f.bindings.WithProber(&fakeProber{live: map[string]bool{
		"10.9.0.2": true,
		"10.9.0.3": true,
		"10.9.0.5": true,
	}})

	ip, err := f.bindings.allocateIPFromSubnet(context.Background(), "machine-x", allocSubnet, "10.9.0.1")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if ip != "10.9.0.4" {
		t.Errorf("allocated %s, want 10.9.0.4 (first silent after live .2/.3)", ip)
	}
}

// TestAllocateProbesInOrder: the prober must be asked about candidates in
// ascending order, in batches.
func TestAllocateProbesInOrder(t *testing.T) {
	f := newFixture(t)
	fp := &fakeProber{live: map[string]bool{"10.9.0.2": true, "10.9.0.3": true}}
	f.bindings.WithProber(fp)

	if _, err := f.bindings.allocateIPFromSubnet(context.Background(), "m", allocSubnet, "10.9.0.1"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if len(fp.probes) != 1 {
		t.Fatalf("probe batches = %d, want 1", len(fp.probes))
	}
	// One window carries the whole first batch, ascending from network+1.
	want := []string{"10.9.0.2", "10.9.0.3", "10.9.0.4"}
	got := fp.probes[0]
	if len(got) < len(want) {
		t.Fatalf("probe batch = %v, want at least %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("probe batch = %v, want ascending prefix %v", got, want)
		}
	}
}

// TestAllocateSkipsLeases: non-expired DHCP leases are off-limits.
func TestAllocateSkipsLeases(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Seed the leases table directly (it is created by the leases package in
	// production; here a bare CREATE keeps the test self-contained).
	if _, err := f.db.ExecContext(ctx, `
        CREATE TABLE IF NOT EXISTS leases (
            mac TEXT PRIMARY KEY, ip TEXT NOT NULL UNIQUE, hostname TEXT NOT NULL DEFAULT '',
            state TEXT NOT NULL, expires_at INTEGER NOT NULL, created_at INTEGER NOT NULL,
            updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for i, ip := range []string{"10.9.0.2", "10.9.0.3"} {
		if _, err := f.db.ExecContext(ctx,
			`INSERT INTO leases (mac, ip, state, expires_at, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
			"aa:bb:cc:dd:ee:0"+string(rune('0'+i)), ip, "active", now+3600, now, now); err != nil {
			t.Fatal(err)
		}
	}
	// An EXPIRED lease must not block the address.
	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO leases (mac, ip, state, expires_at, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		"aa:bb:cc:dd:ee:09", "10.9.0.5", "released", now-3600, now, now); err != nil {
		t.Fatal(err)
	}

	ip, err := f.bindings.allocateIPFromSubnet(ctx, "m", allocSubnet, "10.9.0.1")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if ip != "10.9.0.4" {
		t.Errorf("allocated %s, want 10.9.0.4 (leases .2/.3 held, expired .5 free)", ip)
	}
}

// TestAllocateSkipsDHCPPool: with full DHCP mode, the pool range is not
// eligible for static allocation even though no lease covers it.
func TestAllocateSkipsDHCPPool(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.db.ExecContext(ctx, `
        CREATE TABLE IF NOT EXISTS settings (
            key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL,
            updated_by TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, kv := range [][2]string{
		{"dhcp.mode", "full"},
		{"dhcp.pool.start", "10.9.0.2"},
		{"dhcp.pool.end", "10.9.0.10"},
	} {
		if _, err := f.db.ExecContext(ctx,
			`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?,?,?)`,
			kv[0], kv[1], now); err != nil {
			t.Fatal(err)
		}
	}

	ip, err := f.bindings.allocateIPFromSubnet(ctx, "m", allocSubnet, "10.9.0.1")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if ip != "10.9.0.11" {
		t.Errorf("allocated %s, want 10.9.0.11 (pool .2-.10 excluded)", ip)
	}
}

// TestAllocateIgnoresPoolInProxyMode: proxy mode has no allocation pool, so
// stale settings rows must not block addresses.
func TestAllocateIgnoresPoolInProxyMode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.db.ExecContext(ctx, `
        CREATE TABLE IF NOT EXISTS settings (
            key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL,
            updated_by TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, kv := range [][2]string{
		{"dhcp.mode", "proxy"},
		{"dhcp.pool.start", "10.9.0.2"},
		{"dhcp.pool.end", "10.9.0.30"},
	} {
		if _, err := f.db.ExecContext(ctx,
			`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?,?,?)`,
			kv[0], kv[1], now); err != nil {
			t.Fatal(err)
		}
	}
	ip, err := f.bindings.allocateIPFromSubnet(ctx, "m", allocSubnet, "10.9.0.1")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if ip != "10.9.0.2" {
		t.Errorf("allocated %s, want 10.9.0.2 (proxy mode: pool rows ignored)", ip)
	}
}

// TestAllocateStickyKeepsOwnAddress: re-saving a binding without an explicit
// address must not move the machine.
func TestAllocateStickyKeepsOwnAddress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mu := f.seedMachine(t, 'a')
	im := f.seedImage(t, "a")
	pr := f.seedProfile(t, "p-sticky", "static")
	sid := f.seedSubnet(t, "lab-sticky", allocSubnet, "10.9.0.1")
	if _, err := f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID:   mu,
		ImageID:       im,
		ProfileID:     pr,
		DesiredState:  "install",
		StaticAddress: "10.9.0.77",
		SubnetID:      &sid,
		UpdatedBy:     "admin",
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	ip, err := f.bindings.allocateIPFromSubnet(ctx, mu, allocSubnet, "10.9.0.1")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if ip != "10.9.0.77" {
		t.Errorf("allocated %s, want the sticky 10.9.0.77", ip)
	}
}

// TestAllocateProbeFailureFallsBack: an unusable prober (no interface in the
// subnet, no CAP_NET_RAW) degrades to the first unreserved candidate.
func TestAllocateProbeFailureFallsBack(t *testing.T) {
	f := newFixture(t)
	f.bindings.WithProber(&fakeProber{err: errors.New("no local interface covers 10.9.0.2")})

	ip, err := f.bindings.allocateIPFromSubnet(context.Background(), "m", allocSubnet, "10.9.0.1")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if ip != "10.9.0.2" {
		t.Errorf("allocated %s, want 10.9.0.2 (unprobed fallback)", ip)
	}
}

// TestAllocateAllLiveFails: when every candidate answers, allocation fails
// rather than handing out a taken address.
func TestAllocateAllLiveFails(t *testing.T) {
	f := newFixture(t)
	live := map[string]bool{}
	for i := 2; i <= 254; i++ {
		live[net.IPv4(10, 9, 0, byte(i)).String()] = true
	}
	f.bindings.WithProber(&fakeProber{live: live})

	_, err := f.bindings.allocateIPFromSubnet(context.Background(), "m", allocSubnet, "10.9.0.1")
	if err == nil {
		t.Fatal("expected an error when every candidate is live")
	}
	if !strings.Contains(err.Error(), "answered ARP") {
		t.Errorf("error = %v, want mention of ARP", err)
	}
}

// TestAllocateExcludesOtherBindings: other bindings' reservations still win
// over an otherwise free address.
func TestAllocateExcludesOtherBindings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mu := f.seedMachine(t, 'b')
	im := f.seedImage(t, "b")
	pr := f.seedProfile(t, "p-other", "static")
	sid := f.seedSubnet(t, "lab-other", allocSubnet, "10.9.0.1")
	if _, err := f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID:   mu,
		ImageID:       im,
		ProfileID:     pr,
		DesiredState:  "install",
		StaticAddress: "10.9.0.2",
		SubnetID:      &sid,
		UpdatedBy:     "admin",
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	ip, err := f.bindings.allocateIPFromSubnet(ctx, "some-other-machine", allocSubnet, "10.9.0.1")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if ip != "10.9.0.3" {
		t.Errorf("allocated %s, want 10.9.0.3", ip)
	}
}
