package bindings

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"metalkit/internal/sshkeys"
)

// ssh_key_ids three-state persistence, same semantics as hostname:
// nil keeps the stored selection (job page retry re-arms with only
// image/profile/desired_state and must not un-assign keys), a non-nil
// slice replaces it, an empty slice clears. Unknown ids and malformed ids
// are rejected and leave the row untouched.
func TestSSHKeyIDsThreeState(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mu := f.seedMachine(t, '7')
	im := f.seedImage(t, "e")
	pr := f.seedProfile(t, "p-ssh", "dhcp")

	k1 := f.seedSSHKey(t, "ops-main")
	k2 := f.seedSSHKey(t, "ops-backup")

	in := UpsertInput{
		MachineUUID:  mu,
		ImageID:      im,
		ProfileID:    pr,
		DesiredState: "install",
		UpdatedBy:    "admin",
	}

	// Fresh row: no selection.
	b, err := f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(fresh): %v", err)
	}
	if len(b.SSHKeyIDs) != 0 {
		t.Errorf("fresh binding ssh_key_ids = %v, want empty", b.SSHKeyIDs)
	}

	// Set two keys.
	in.SSHKeyIDs = &[]string{k1, k2}
	b, err = f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(set): %v", err)
	}
	if len(b.SSHKeyIDs) != 2 || b.SSHKeyIDs[0] != k1 || b.SSHKeyIDs[1] != k2 {
		t.Fatalf("ssh_key_ids after set = %v, want [%s %s]", b.SSHKeyIDs, k1, k2)
	}

	// nil on a later PUT keeps the stored selection (the retry path).
	in.SSHKeyIDs = nil
	in.DesiredState = "reinstall"
	b, err = f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(keep): %v", err)
	}
	if len(b.SSHKeyIDs) != 2 {
		t.Fatalf("ssh_key_ids after keep upsert = %v, want 2 keys", b.SSHKeyIDs)
	}

	// List agrees.
	list, err := f.bindings.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, lb := range list {
		if lb.MachineUUID == mu && len(lb.SSHKeyIDs) != 2 {
			t.Errorf("List ssh_key_ids = %v, want 2 keys", lb.SSHKeyIDs)
		}
	}

	// Partial replace keeps selection exact, not additive.
	in.SSHKeyIDs = &[]string{k2}
	b, err = f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(replace): %v", err)
	}
	if len(b.SSHKeyIDs) != 1 || b.SSHKeyIDs[0] != k2 {
		t.Fatalf("ssh_key_ids after replace = %v, want [%s]", b.SSHKeyIDs, k2)
	}

	// Explicit clear.
	in.SSHKeyIDs = &[]string{}
	b, err = f.bindings.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("upsert(clear): %v", err)
	}
	if len(b.SSHKeyIDs) != 0 {
		t.Fatalf("ssh_key_ids after clear = %v, want empty", b.SSHKeyIDs)
	}

	// Unknown id: rejected, row untouched (Get still returns the cleared state).
	in.SSHKeyIDs = &[]string{"00000000000000000000000000000000"}
	if _, err := f.bindings.Upsert(ctx, in); err == nil || !strings.Contains(err.Error(), "ssh_key") {
		t.Errorf("unknown ssh key id: want error, got %v", err)
	}
	got, err := f.bindings.Get(ctx, mu)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.SSHKeyIDs) != 0 {
		t.Errorf("rejected upsert changed the row: %v", got.SSHKeyIDs)
	}

	// Malformed id: rejected.
	in.SSHKeyIDs = &[]string{"nothex"}
	if _, err := f.bindings.Upsert(ctx, in); err == nil {
		t.Errorf("malformed ssh key id accepted")
	}
}

func TestRefCountBySSHKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mu := f.seedMachine(t, '8')
	mu2 := f.seedMachine(t, '9')
	im := f.seedImage(t, "f")
	pr := f.seedProfile(t, "p-ref", "dhcp")
	k1 := f.seedSSHKey(t, "ref-key-1")
	k2 := f.seedSSHKey(t, "ref-key-2")

	base := UpsertInput{ImageID: im, ProfileID: pr, DesiredState: "install", UpdatedBy: "admin"}
	in := base
	in.MachineUUID = mu
	in.SSHKeyIDs = &[]string{k1, k2}
	if _, err := f.bindings.Upsert(ctx, in); err != nil {
		t.Fatalf("upsert m1: %v", err)
	}
	in = base
	in.MachineUUID = mu2
	in.SSHKeyIDs = &[]string{k1}
	if _, err := f.bindings.Upsert(ctx, in); err != nil {
		t.Fatalf("upsert m2: %v", err)
	}

	if n, err := f.bindings.RefCountBySSHKey(ctx, k1); err != nil || n != 2 {
		t.Errorf("RefCountBySSHKey(k1) = %d, %v; want 2, nil", n, err)
	}
	if n, err := f.bindings.RefCountBySSHKey(ctx, k2); err != nil || n != 1 {
		t.Errorf("RefCountBySSHKey(k2) = %d, %v; want 1, nil", n, err)
	}
	// An id that merely *looks like* it could substring-match must not
	// false-positive: build an id sharing a long prefix with k1.
	klookalike := "b" + k1[1:]
	if n, err := f.bindings.RefCountBySSHKey(ctx, klookalike); err != nil || n != 0 {
		t.Errorf("RefCountBySSHKey(lookalike) = %d, %v; want 0, nil", n, err)
	}
}

// TestAPIUpsertUnknownSSHKey: 合法格式但不存在的 id（不在 ssh_keys 表）→ 422，
// 与未知 image/profile/subnet 同一档（外键缺失，不是格式错误）。
func TestAPIUpsertUnknownSSHKey(t *testing.T) {
	f, ts := newTestAPI(t)
	mu := f.seedMachine(t, '4')
	im := f.seedImage(t, "a")
	pr := f.seedProfile(t, "p-api", "dhcp")
	resp, _ := doRequest(t, http.MethodPut, ts.URL+"/api/v1/bindings/"+mu, map[string]any{
		"image_id":    im,
		"profile_id":  pr,
		"desired_state": "install",
		"ssh_key_ids": []string{"00000000000000000000000000000000"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func (f *testFixture) seedSSHKey(t *testing.T, name string) string {
	t.Helper()
	k, err := f.sshkeys.Create(context.Background(), sshkeys.CreateInput{
		Name:      name,
		PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIEeIM72OY68FIhL6i3KsXruAYy0C5matj1jacX/rDwd " + name + "@metalkit",
		CreatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("seed ssh key %s: %v", name, err)
	}
	return k.ID
}
