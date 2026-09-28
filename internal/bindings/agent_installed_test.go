package bindings

import (
	"context"
	"testing"
)

// agent_installed_override three-state persistence: nil keeps, true stores
// 1, false stores 0 (not NULL), and Get surfaces them as *bool (nil =
// inherit profile).
func TestAgentInstalledOverrideThreeState(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mu := f.seedMachine(t, '9')
	im := f.seedImage(t, "e")
	pr := f.seedProfile(t, "p-mon", "dhcp")

	// Fresh row: no override.
	in := UpsertInput{
		MachineUUID:  mu,
		ImageID:      im,
		ProfileID:    pr,
		DesiredState: "install",
		UpdatedBy:    "admin",
	}
	b, err := f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if b.AgentInstalledOverride != nil {
		t.Errorf("fresh binding should inherit (nil), got %v", *b.AgentInstalledOverride)
	}

	// Set true.
	tOn := true
	in.AgentInstalledOverride = &tOn
	b, err = f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(true): %v", err)
	}
	if b.AgentInstalledOverride == nil || !*b.AgentInstalledOverride {
		t.Error("override=true not persisted")
	}

	// Flip to false — stored as 0, still set (distinguishable from NULL).
	tOff := false
	in.AgentInstalledOverride = &tOff
	b, err = f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(false): %v", err)
	}
	if b.AgentInstalledOverride == nil || *b.AgentInstalledOverride {
		t.Error("override=false not persisted (or read back as true)")
	}

	// nil keeps the stored false (does NOT clear back to NULL).
	in.AgentInstalledOverride = nil
	b, err = f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(nil): %v", err)
	}
	if b.AgentInstalledOverride == nil || *b.AgentInstalledOverride {
		t.Error("nil update should keep the stored false override")
	}

	// List sees the same.
	list, err := f.bindings.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, lb := range list {
		if lb.MachineUUID == mu {
			if lb.AgentInstalledOverride == nil || *lb.AgentInstalledOverride {
				t.Error("List lost the false override")
			}
			return
		}
	}
	t.Fatal("binding missing from List")
}
