package profiles

import (
	"context"
	"testing"
)

// agent_installed round-trip: create stores it, Get/List echo it, Update's
// three-state *bool toggles it.
func TestAgentInstalledRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	in := validInput("mon")
	p, err := s.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.AgentInstalled {
		t.Error("default AgentInstalled should be false")
	}

	// Create with the flag on.
	on := validInput("mon-on")
	on.AgentInstalled = true
	pOn, err := s.Create(ctx, on)
	if err != nil {
		t.Fatalf("Create(on): %v", err)
	}
	if !pOn.AgentInstalled {
		t.Error("AgentInstalled=true not persisted by Create")
	}

	// Get / List echo it.
	got, err := s.Get(ctx, pOn.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.AgentInstalled {
		t.Error("Get lost AgentInstalled")
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, lp := range list {
		if lp.ID == pOn.ID && lp.AgentInstalled {
			found = true
		}
	}
	if !found {
		t.Error("List lost AgentInstalled")
	}

	// Update: nil keeps, true sets, false clears.
	upd, err := s.Update(ctx, p.ID, UpdateInput{})
	if err != nil {
		t.Fatalf("Update(nil): %v", err)
	}
	if upd.AgentInstalled {
		t.Error("nil update should keep AgentInstalled=false")
	}
	tOn := true
	upd, err = s.Update(ctx, p.ID, UpdateInput{AgentInstalled: &tOn})
	if err != nil {
		t.Fatalf("Update(true): %v", err)
	}
	if !upd.AgentInstalled {
		t.Error("Update(true) did not set AgentInstalled")
	}
	tOff := false
	upd, err = s.Update(ctx, p.ID, UpdateInput{AgentInstalled: &tOff})
	if err != nil {
		t.Fatalf("Update(false): %v", err)
	}
	if upd.AgentInstalled {
		t.Error("Update(false) did not clear AgentInstalled")
	}

	// The value survives a store re-open (migration-col round-trip).
	got2, err := s.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("re-Get: %v", err)
	}
	if got2.AgentInstalled {
		t.Error("AgentInstalled not durable")
	}
}
