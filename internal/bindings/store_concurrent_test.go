package bindings

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestUpsert_ConcurrentAutoAllocate is the regression test for the allocation
// race: two concurrent Upserts for different machines in the same subnet used
// to both read the same "free" IP (the read-reserved→probe→INSERT sequence
// was unsynchronised) and each won its own row's INSERT — leaving two hosts
// with the same static_address. Mirrors leases.TestAllocate_ConcurrentBurst.
func TestUpsert_ConcurrentAutoAllocate(t *testing.T) {
	f := newFixture(t)
	uuid1 := f.seedMachine(t, '1')
	uuid2 := f.seedMachine(t, '2')
	uuid3 := f.seedMachine(t, '3')
	image := f.seedImage(t, "1")
	profile := f.seedProfile(t, "static-with-subnet", "static")
	subnet := f.seedSubnet(t, "race", "10.90.0.0/24", "10.90.0.1")

	// Lines up the three goroutines at the same instant so the check-then-act
	// windows genuinely overlap (a bare go func{} racily passes even unlocked).
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex // guards addrs / errs
	addrs := map[string]string{}
	errs := map[string]error{}

	upsert := func(uuid string) {
		defer wg.Done()
		<-start
		b, err := f.bindings.Upsert(context.Background(), UpsertInput{
			MachineUUID:  uuid,
			ImageID:      image,
			ProfileID:    profile,
			SubnetID:     &subnet,
			DesiredState: "install",
			UpdatedBy:    "qa",
		})
		mu.Lock()
		defer mu.Unlock()
		errs[uuid] = err
		if err == nil {
			addrs[uuid] = b.StaticAddress
		}
	}

	wg.Add(3)
	go upsert(uuid1)
	go upsert(uuid2)
	go upsert(uuid3)
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent Upserts did not finish within 30s (deadlock?)")
	}

	for uuid, err := range errs {
		if err != nil {
			t.Errorf("Upsert for %s failed: %v", uuid, err)
		}
	}
	if len(addrs) != 3 {
		t.Fatalf("expected 3 successful upserts, got %d", len(addrs))
	}
	seen := map[string]string{}
	for uuid, addr := range addrs {
		if first, dup := seen[addr]; dup {
			t.Errorf("machines %s and %s were auto-allocated the same address %s",
				first, uuid, addr)
		}
		seen[addr] = uuid
	}
}
