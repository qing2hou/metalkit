// Command probecheck verifies ARP-based liveness probing and the static-IP
// allocation path against the live network. Run on the metalkit host:
//
//	./probecheck <db-copy.sqlite>
//
// It touches only the DB copy passed in — never the production database.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"metalkit/internal/bindings"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: probecheck <db-copy.sqlite>")
		os.Exit(2)
	}
	dbPath := os.Args[1]
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	fmt.Println("=== 1a. ARP liveness probe: known-live vs unused ===")
	prober := &bindings.ARPProber{Timeout: 900 * time.Millisecond}
	// Note: the controller's own address (.11) is deliberately NOT here — a
	// host never answers ARP for locally-originated requests (verified: arping
	// from the same host also gets 0 replies). Own addresses are excluded
	// before probing, via the local-interface enumeration.
	live := []string{"192.168.10.1", "192.168.10.150", "192.168.10.110"}
	dead := []string{"192.168.10.240", "192.168.10.241", "192.168.10.242"}
	var ips []net.IP
	for _, s := range append(append([]string{}, live...), dead...) {
		ips = append(ips, net.ParseIP(s))
	}
	got, err := prober.InUseBatch(context.Background(), ips)
	if err != nil {
		fmt.Printf("probe error: %v\n", err)
		os.Exit(1)
	}
	fail := false
	for _, s := range live {
		alive := got[s]
		mark := "OK"
		if !alive {
			mark = "MISS (expected alive)"
			fail = true
		}
		fmt.Printf("  %-16s alive=%-5v  [%s]\n", s, alive, mark)
	}
	for _, s := range dead {
		alive := got[s]
		mark := "OK"
		if alive {
			mark = "FALSE-POSITIVE (unexpected)"
			fail = true
		}
		fmt.Printf("  %-16s alive=%-5v  [%s]\n", s, alive, mark)
	}

	fmt.Println()
	fmt.Println("=== 1b. ICMP liveness probe: known-live vs unused ===")
	icmpProber := &bindings.ICMPProber{Timeout: 1200 * time.Millisecond}
	igot, ierr := icmpProber.InUseBatch(context.Background(), ips)
	if ierr != nil {
		fmt.Printf("icmp probe error: %v\n", ierr)
		fail = true
	} else {
		for _, s := range live {
			alive := igot[s]
			mark := "OK"
			if !alive {
				mark = "no ICMP reply (host may block ping — expected for some)"
			}
			fmt.Printf("  %-16s alive=%-5v  [%s]\n", s, alive, mark)
		}
		for _, s := range dead {
			alive := igot[s]
			mark := "OK"
			if alive {
				mark = "FALSE-POSITIVE (unexpected)"
				fail = true
			}
			fmt.Printf("  %-16s alive=%-5v  [%s]\n", s, alive, mark)
		}
	}

	fmt.Println()
	fmt.Println("=== 2. Allocation path against a DB copy (composite ARP+ICMP prober) ===")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=rw")
	if err != nil {
		fmt.Printf("open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	store, err := bindings.NewStore(context.Background(), db, logger, nil)
	if err != nil {
		fmt.Printf("bindings.NewStore: %v\n", err)
		os.Exit(1)
	}
	store.WithProber(&bindings.CompositeProber{ARP: prober, ICMP: icmpProber, Logger: logger})

	// Pick the real machine (any row) — we only exercise allocation.
	var machineUUID string
	if err := db.QueryRow(`SELECT uuid FROM machines LIMIT 1`).Scan(&machineUUID); err != nil {
		fmt.Printf("no machine in copy: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  machine: %s\n", machineUUID)

	// Pre-reserve 192.168.10.2 .. 192.168.10.109 as if other bindings held
	// them, so allocation must walk up to .110 (live box) and beyond.
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		fmt.Printf("pragma: %v\n", err)
		os.Exit(1)
	}
	now := time.Now().Unix()
	reserved := 0
	for i := 2; i <= 109; i++ {
		ip := fmt.Sprintf("192.168.10.%d", i)
		if _, err := db.Exec(
			`INSERT OR REPLACE INTO bindings (machine_uuid, image_id, profile_id, desired_state, static_address, updated_at, updated_by)
             VALUES (?, 'x', 'y', 'install', ?, ?, 'probecheck')`, fmt.Sprintf("fake-%d", i), ip, now); err != nil {
			fmt.Printf("reserve %s: %v\n", ip, err)
			os.Exit(1)
		}
		reserved++
	}
	fmt.Printf("  pre-reserved .2-.109 via %d fake bindings\n", reserved)

	// Call the allocator through a real Upsert so the full path runs.
	subnetID, err := ensureSubnet(db, "probecheck-subnet", "192.168.10.0/24", "192.168.10.1")
	if err != nil {
		fmt.Printf("subnet: %v\n", err)
		os.Exit(1)
	}
	profID, err := ensureStaticProfile(db, machineUUID)
	if err != nil {
		fmt.Printf("profile: %v\n", err)
		os.Exit(1)
	}
	var imageID string
	if err := db.QueryRow(`SELECT id FROM images LIMIT 1`).Scan(&imageID); err != nil {
		fmt.Printf("no image in copy: %v\n", err)
		os.Exit(1)
	}
	b, err := store.Upsert(context.Background(), bindings.UpsertInput{
		MachineUUID:  machineUUID,
		ImageID:      imageID,
		ProfileID:    profID,
		DesiredState: "install",
		SubnetID:     &subnetID,
		UpdatedBy:    "probecheck",
	})
	if err != nil {
		fmt.Printf("Upsert (allocation) failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  allocated: %s\n", b.StaticAddress)
	fmt.Println()

	probe110 := got["192.168.10.110"]
	ok := true
	if probe110 {
		fmt.Println("  .110 is live on the LAN — allocation MUST NOT have picked it")
		if b.StaticAddress == "192.168.10.110" {
			fmt.Println("  FAIL: allocated a live address (ARP probe ignored)")
			ok = false
		} else {
			fmt.Printf("  OK: skipped live .110, landed on %s\n", b.StaticAddress)
		}
	} else {
		fmt.Println("  note: .110 was silent during probing (host off) — live-skip not exercised here")
	}
	if b.StaticAddress == "192.168.10.11" {
		fmt.Println("  FAIL: allocated the controller's own address")
		ok = false
	}
	if b.StaticAddress == "" {
		fmt.Println("  FAIL: no address allocated")
		ok = false
	}
	if fail {
		fmt.Println()
		fmt.Println("RESULT: probe primitive FAILED")
		os.Exit(1)
	}
	if !ok {
		fmt.Println()
		fmt.Println("RESULT: allocation FAILED")
		os.Exit(1)
	}
	fmt.Println()
	fmt.Println("RESULT: all checks passed")
}

// ensureSubnet inserts a subnet row if absent and returns its id.
func ensureSubnet(db *sql.DB, name, cidr, gw string) (string, error) {
	var id string
	err := db.QueryRow(`SELECT id FROM subnets WHERE name = ?`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	id = "abcdef0123456789abcdef0123456789"
	now := time.Now().Unix()
	_, err = db.Exec(
		`INSERT INTO subnets (id, name, description, cidr, gateway, dns_json, created_at, updated_at, created_by, updated_by)
         VALUES (?, ?, '', ?, ?, '["1.1.1.1"]', ?, ?, 'probecheck', 'probecheck')`,
		id, name, cidr, gw, now, now)
	return id, err
}

// ensureStaticProfile rewrites the machine's profile-agnostic path: it picks
// any profile row and forces method=static on a copy under a new id.
func ensureStaticProfile(db *sql.DB, _ string) (string, error) {
	var id, networkJSON string
	err := db.QueryRow(`SELECT id, network_json FROM profiles LIMIT 1`).Scan(&id, &networkJSON)
	if err != nil {
		return "", fmt.Errorf("no profile row: %w", err)
	}
	newID := "0123456789abcdef0123456789abcdef"
	_, err = db.Exec(
		`INSERT OR REPLACE INTO profiles (id, name, description, hostname_template, root_password_hash, target_disk_json, network_json, os_family, subnet_id, created_at, updated_at, created_by)
         SELECT ?, name || '-probecheck', description, hostname_template, root_password_hash, target_disk_json, '{"method":"static","nic_selector":"auto"}', os_family, '', created_at, updated_at, 'probecheck'
         FROM profiles WHERE id = ?`, newID, id)
	return newID, err
}
