package bindings

import (
	"context"
	"strings"
	"testing"
)

// hostname three-state persistence: nil keeps the stored override, a
// non-empty value replaces it, and "" clears it back to NULL (the agent then
// expands profile.hostname_template). The nil-keeps case is the one that
// matters in practice — the job page's "retry" re-arms a binding with only
// image/profile/desired_state, and a plain string field used to wipe the
// custom hostname on every retry.
func TestHostnameOverrideThreeState(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mu := f.seedMachine(t, '3')
	im := f.seedImage(t, "d")
	pr := f.seedProfile(t, "p-host", "dhcp")

	in := UpsertInput{
		MachineUUID:  mu,
		ImageID:      im,
		ProfileID:    pr,
		DesiredState: "install",
		UpdatedBy:    "admin",
	}

	// Fresh row: no override.
	b, err := f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(fresh): %v", err)
	}
	if b.Hostname != "" {
		t.Errorf("fresh binding hostname = %q, want empty", b.Hostname)
	}

	// Set.
	in.Hostname = ptrStr("node-007")
	b, err = f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(set): %v", err)
	}
	if b.Hostname != "node-007" {
		t.Fatalf("hostname after set = %q, want node-007", b.Hostname)
	}

	// nil on a later PUT keeps the stored value (the retry path).
	in.Hostname = nil
	in.DesiredState = "reinstall"
	b, err = f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(keep): %v", err)
	}
	if b.Hostname != "node-007" {
		t.Fatalf("hostname after keep upsert = %q, want node-007", b.Hostname)
	}

	// List agrees.
	list, err := f.bindings.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, lb := range list {
		if lb.MachineUUID == mu && lb.Hostname != "node-007" {
			t.Errorf("List hostname = %q, want node-007", lb.Hostname)
		}
	}

	// Explicit clear.
	in.Hostname = ptrStr("")
	b, err = f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(clear): %v", err)
	}
	if b.Hostname != "" {
		t.Fatalf("hostname after clear = %q, want empty", b.Hostname)
	}

	// Clearing twice is a no-op, not an error.
	b, err = f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(clear again): %v", err)
	}
	if b.Hostname != "" {
		t.Fatalf("hostname after second clear = %q, want empty", b.Hostname)
	}

	// Invalid values are rejected and leave the row untouched.
	for _, bad := range []string{"host!name", "-leading", "trailing-", "a..b", strings.Repeat("a", 64)} {
		if _, err := f.bindings.Upsert(ctx, UpsertInput{
			MachineUUID: mu, ImageID: im, ProfileID: pr,
			DesiredState: "install", UpdatedBy: "admin",
			Hostname: ptrStr(bad),
		}); err == nil || !strings.Contains(err.Error(), "hostname") {
			t.Errorf("hostname %q: want hostname validation error, got %v", bad, err)
		}
	}
}
