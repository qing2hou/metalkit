package bindings

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metalkit/internal/crypto"
	"metalkit/internal/images"
	"metalkit/internal/inventory"
	"metalkit/internal/profiles"
	"metalkit/internal/sqlitedb"
)

// testFixture holds the four stores a binding test needs.
type testFixture struct {
	db        *sql.DB
	machines  *inventory.Store
	images    *images.Store
	profiles  *profiles.Store
	bindings  *Store
}

func newFixture(t *testing.T) *testFixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{Path: path, Logger: logger})
	if err != nil {
		t.Fatalf("sqlitedb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	inv, err := inventory.NewStore(context.Background(), db, logger)
	if err != nil {
		t.Fatalf("inventory.NewStore: %v", err)
	}
	imgStore, err := images.NewStore(context.Background(), db, logger, t.TempDir())
	if err != nil {
		t.Fatalf("images.NewStore: %v", err)
	}
	profStore, err := profiles.NewStore(context.Background(), db, logger)
	if err != nil {
		t.Fatalf("profiles.NewStore: %v", err)
	}
	bindStore, err := NewStore(context.Background(), db, logger, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return &testFixture{
		db:       db,
		machines: inv,
		images:   imgStore,
		profiles: profStore,
		bindings: bindStore,
	}
}

const validSha512crypt = `$6$rounds=4096$abcdefghijklmnop$` +
	`aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`

// seedMachine inserts a minimal machines row by issuing raw SQL — avoids
// constructing a full inventory.Report. UUIDs are deterministic by suffix.
func (f *testFixture) seedMachine(t *testing.T, suffix byte) string {
	t.Helper()
	uuid := "4c4c4544-0058-3210-8053-c5c04f46383" + string(suffix)
	now := time.Now().Unix()
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO machines (uuid, first_seen, last_seen, status, latest_report)
         VALUES (?, ?, ?, 'online', NULL)`, uuid, now, now); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	return uuid
}

// seedProfile creates a profile of the requested network method and returns id.
func (f *testFixture) seedProfile(t *testing.T, name, method string) string {
	t.Helper()
	td, _ := json.Marshal(profiles.TargetDisk{Mode: "smallest"})
	var nc profiles.NetworkConfig
	switch method {
	case "static":
		nc = profiles.NetworkConfig{
			Method: "static", PrefixLen: 24, Gateway: "10.0.0.1",
			DNS: []string{"1.1.1.1"}, NICSelector: "auto",
		}
	case "dhcp":
		nc = profiles.NetworkConfig{Method: "dhcp", NICSelector: "auto"}
	default:
		t.Fatalf("bad method %q", method)
	}
	ncBlob, _ := json.Marshal(nc)
	p, err := f.profiles.Create(context.Background(), profiles.CreateInput{
		Name:             name,
		HostnameTemplate: "node-{serial}",
		RootPasswordHash: validSha512crypt,
		TargetDisk:       td,
		Network:          ncBlob,
		CreatedBy:        "admin",
	})
	if err != nil {
		t.Fatalf("seed profile %s: %v", name, err)
	}
	return p.ID
}

// seedImage inserts a finalized image row by going through the store's
// FinalizeImage so the row passes its own schema validation. The suffix is
// taken as the first character and used to build a 64-hex-char sha256 — it
// MUST be a hex digit (0-9, a-f).
func (f *testFixture) seedImage(t *testing.T, suffix string) string {
	t.Helper()
	if len(suffix) != 1 || !strings.ContainsRune("0123456789abcdef", rune(suffix[0])) {
		t.Fatalf("seedImage suffix %q must be a single hex digit", suffix)
	}
	sha := strings.Repeat(suffix, 64)
	img, err := f.images.FinalizeImage(context.Background(), images.FinalizeInput{
		Name:        "test-" + suffix + ".qcow2",
		Format:      "qcow2",
		SizeBytes:   1024,
		VirtualSize: 4096,
		SHA256:      sha,
		UploadedBy:  "admin",
	})
	if err != nil {
		t.Fatalf("seed image: %v", err)
	}
	return img.ID
}

func TestUpsertHappyStatic(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '0')
	im := f.seedImage(t, "a")
	pr := f.seedProfile(t, "p-static", "static")

	b, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID:   mu,
		ImageID:       im,
		ProfileID:     pr,
		DesiredState:  "install",
		StaticAddress: "10.0.0.42",
		Hostname:      "host42",
		UpdatedBy:     "admin",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if b.MachineUUID != mu || b.ImageID != im || b.ProfileID != pr {
		t.Errorf("refs mismatch: %+v", b)
	}
	if b.StaticAddress != "10.0.0.42" || b.Hostname != "host42" || b.DesiredState != "install" {
		t.Errorf("fields mismatch: %+v", b)
	}
}

func TestUpsertHappyDHCP(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '1')
	im := f.seedImage(t, "b")
	pr := f.seedProfile(t, "p-dhcp", "dhcp")

	b, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID:  mu,
		ImageID:      im,
		ProfileID:    pr,
		DesiredState: "install",
		UpdatedBy:    "admin",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if b.StaticAddress != "" {
		t.Errorf("dhcp binding leaked address: %q", b.StaticAddress)
	}
}

func TestUpsertStaticRequiresAddress(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '2')
	im := f.seedImage(t, "c")
	pr := f.seedProfile(t, "p-static-2", "static")

	_, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install",
		UpdatedBy:    "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "static_address") {
		t.Fatalf("want static_address error, got %v", err)
	}
}

func TestUpsertDHCPRejectsAddress(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '3')
	im := f.seedImage(t, "d")
	pr := f.seedProfile(t, "p-dhcp-2", "dhcp")

	_, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID:   mu, ImageID: im, ProfileID: pr,
		DesiredState:  "install",
		StaticAddress: "10.0.0.5",
		UpdatedBy:     "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "static_address") {
		t.Fatalf("want static_address error, got %v", err)
	}
}

func TestUpsertUnknownMachine(t *testing.T) {
	f := newFixture(t)
	im := f.seedImage(t, "e")
	pr := f.seedProfile(t, "p-um", "dhcp")

	_, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID:  "ffffffff-ffff-ffff-ffff-ffffffffffff",
		ImageID:      im,
		ProfileID:    pr,
		DesiredState: "install",
		UpdatedBy:    "admin",
	})
	if !errors.Is(err, ErrMachineUnknown) {
		t.Fatalf("want ErrMachineUnknown, got %v", err)
	}
}

func TestUpsertUnknownImage(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '4')
	pr := f.seedProfile(t, "p-ui", "dhcp")

	_, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID:  mu,
		ImageID:      "00000000000000000000000000000000",
		ProfileID:    pr,
		DesiredState: "install",
		UpdatedBy:    "admin",
	})
	if !errors.Is(err, ErrImageUnknown) {
		t.Fatalf("want ErrImageUnknown, got %v", err)
	}
}

func TestUpsertUnknownProfile(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '5')
	im := f.seedImage(t, "2")

	_, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID:  mu,
		ImageID:      im,
		ProfileID:    "00000000000000000000000000000000",
		DesiredState: "install",
		UpdatedBy:    "admin",
	})
	if !errors.Is(err, ErrProfileUnknown) {
		t.Fatalf("want ErrProfileUnknown, got %v", err)
	}
}

func TestUpsertOverwritesInPlace(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '6')
	im1 := f.seedImage(t, "3")
	im2 := f.seedImage(t, "4")
	pr := f.seedProfile(t, "p-ow", "dhcp")

	if _, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, ImageID: im1, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "alice",
	}); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// Second Upsert with a different image — should overwrite.
	b, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, ImageID: im2, ProfileID: pr,
		DesiredState: "reinstall", UpdatedBy: "bob",
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if b.ImageID != im2 || b.DesiredState != "reinstall" || b.UpdatedBy != "bob" {
		t.Errorf("overwrite mismatch: %+v", b)
	}

	// List should have exactly one row.
	got, err := f.bindings.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("len(got)=%d, want 1 (upsert should not insert duplicate)", len(got))
	}
}

func TestGetAndDelete(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '7')
	im := f.seedImage(t, "5")
	pr := f.seedProfile(t, "p-gd", "dhcp")

	if _, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	b, err := f.bindings.Get(context.Background(), mu)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.MachineUUID != mu {
		t.Errorf("Get returned %+v", b)
	}

	if err := f.bindings.Delete(context.Background(), mu); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.bindings.Get(context.Background(), mu); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: want ErrNotFound, got %v", err)
	}
	if err := f.bindings.Delete(context.Background(), mu); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete: want ErrNotFound, got %v", err)
	}
}

func TestRefCounts(t *testing.T) {
	f := newFixture(t)
	mu1 := f.seedMachine(t, '8')
	mu2 := f.seedMachine(t, '9')
	im := f.seedImage(t, "6")
	pr := f.seedProfile(t, "p-rc", "dhcp")

	if got, err := f.bindings.RefCountByImage(context.Background(), im); err != nil || got != 0 {
		t.Fatalf("initial ref count: got=%d err=%v want 0", got, err)
	}

	for _, mu := range []string{mu1, mu2} {
		if _, err := f.bindings.Upsert(context.Background(), UpsertInput{
			MachineUUID: mu, ImageID: im, ProfileID: pr,
			DesiredState: "install", UpdatedBy: "admin",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	if got, err := f.bindings.RefCountByImage(context.Background(), im); err != nil || got != 2 {
		t.Errorf("image ref count: got=%d err=%v want 2", got, err)
	}
	if got, err := f.bindings.RefCountByProfile(context.Background(), pr); err != nil || got != 2 {
		t.Errorf("profile ref count: got=%d err=%v want 2", got, err)
	}
}

func TestBadInputs(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '0')
	im := f.seedImage(t, "1")
	pr := f.seedProfile(t, "p-bad", "dhcp")
	ctx := context.Background()

	cases := []struct {
		name  string
		mut   func(*UpsertInput)
		wants string
	}{
		{"bad uuid", func(in *UpsertInput) { in.MachineUUID = "not-a-uuid" }, "machine_uuid"},
		{"bad image_id", func(in *UpsertInput) { in.ImageID = "short" }, "image_id"},
		{"bad profile_id", func(in *UpsertInput) { in.ProfileID = "short" }, "profile_id"},
		{"bad desired_state", func(in *UpsertInput) { in.DesiredState = "running" }, "desired_state"},
		{"bad hostname", func(in *UpsertInput) { in.Hostname = "host!name" }, "hostname"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := UpsertInput{
				MachineUUID: mu, ImageID: im, ProfileID: pr,
				DesiredState: "install", UpdatedBy: "admin",
			}
			c.mut(&in)
			_, err := f.bindings.Upsert(ctx, in)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wants)
			}
			if !strings.Contains(err.Error(), c.wants) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wants)
			}
		})
	}
}

// newCipheredFixture wires up the bindings store with a real AES-GCM cipher
// so the password lifecycle can be exercised end-to-end. The master key is a
// throwaway 32 random bytes; never touches disk.
func newCipheredFixture(t *testing.T) *testFixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{Path: path, Logger: logger})
	if err != nil {
		t.Fatalf("sqlitedb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	inv, err := inventory.NewStore(context.Background(), db, logger)
	if err != nil {
		t.Fatalf("inventory.NewStore: %v", err)
	}
	imgStore, err := images.NewStore(context.Background(), db, logger, t.TempDir())
	if err != nil {
		t.Fatalf("images.NewStore: %v", err)
	}
	profStore, err := profiles.NewStore(context.Background(), db, logger)
	if err != nil {
		t.Fatalf("profiles.NewStore: %v", err)
	}

	key := make([]byte, crypto.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	cipher, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("crypto.NewCipher: %v", err)
	}
	bindStore, err := NewStore(context.Background(), db, logger, cipher)
	if err != nil {
		t.Fatalf("NewStore (ciphered): %v", err)
	}
	return &testFixture{
		db:       db,
		machines: inv,
		images:   imgStore,
		profiles: profStore,
		bindings: bindStore,
	}
}

func TestPasswordSetAndGet(t *testing.T) {
	f := newCipheredFixture(t)
	mu := f.seedMachine(t, '0')
	im := f.seedImage(t, "1")
	pr := f.seedProfile(t, "p-pw", "dhcp")
	ctx := context.Background()

	pw := "S3cret-Pa55"
	b, err := f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		RootPassword: &pw,
	})
	if err != nil {
		t.Fatalf("Upsert with password: %v", err)
	}
	if !b.HasPassword {
		t.Fatalf("HasPassword=false, want true")
	}

	got, err := f.bindings.GetPassword(ctx, mu)
	if err != nil {
		t.Fatalf("GetPassword: %v", err)
	}
	if got != pw {
		t.Fatalf("GetPassword=%q want %q", got, pw)
	}

	// PUT again without RootPassword → existing ciphertext preserved.
	b2, err := f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "reinstall", UpdatedBy: "admin",
		// RootPassword left nil
	})
	if err != nil {
		t.Fatalf("Upsert keep password: %v", err)
	}
	if !b2.HasPassword {
		t.Fatalf("HasPassword=false after keep-existing upsert, want true")
	}
	got, _ = f.bindings.GetPassword(ctx, mu)
	if got != pw {
		t.Fatalf("password mutated to %q after keep-existing upsert", got)
	}

	// PUT with empty string RootPassword → clear.
	empty := ""
	b3, err := f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		RootPassword: &empty,
	})
	if err != nil {
		t.Fatalf("Upsert clear password: %v", err)
	}
	if b3.HasPassword {
		t.Fatalf("HasPassword=true after clear, want false")
	}
	_, err = f.bindings.GetPassword(ctx, mu)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPassword after clear: want ErrNotFound, got %v", err)
	}
}

func TestPasswordTooShortRejected(t *testing.T) {
	f := newCipheredFixture(t)
	mu := f.seedMachine(t, '0')
	im := f.seedImage(t, "1")
	pr := f.seedProfile(t, "p-pw2", "dhcp")

	short := "abc"
	_, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		RootPassword: &short,
	})
	if err == nil || !strings.Contains(err.Error(), "root_password") {
		t.Fatalf("want root_password validation error, got %v", err)
	}
}

func TestPasswordRequiresCipher(t *testing.T) {
	// nil-cipher fixture: setting a password must error.
	f := newFixture(t)
	mu := f.seedMachine(t, '0')
	im := f.seedImage(t, "1")
	pr := f.seedProfile(t, "p-pw3", "dhcp")

	pw := "S3cret-Pa55"
	_, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		RootPassword: &pw,
	})
	if err == nil || !strings.Contains(err.Error(), "cipher") {
		t.Fatalf("want cipher-required error, got %v", err)
	}
}

// TestTargetDiskOverrideThreeState exercises the per-binding target_disk
// override knob across its three states: not-supplied (keep), null/empty
// (clear), and a concrete value (set). The corresponding column carries
// either the previous JSON, NULL, or the canonicalised new JSON.
func TestTargetDiskOverrideThreeState(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '0')
	im := f.seedImage(t, "1")
	pr := f.seedProfile(t, "p-td", "dhcp")
	ctx := context.Background()

	// 1) First upsert without override → binding.TargetDisk == nil.
	b, err := f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("Upsert 1: %v", err)
	}
	if b.TargetDisk != nil {
		t.Fatalf("TargetDisk after no-override upsert: got %+v, want nil", b.TargetDisk)
	}

	// 2) Set an override.
	tdRaw, _ := json.Marshal(profiles.TargetDisk{Mode: "by-wwn", Value: "0x500a075119abcdef"})
	b, err = f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		TargetDisk: tdRaw,
	})
	if err != nil {
		t.Fatalf("Upsert 2 (set): %v", err)
	}
	if b.TargetDisk == nil || b.TargetDisk.Mode != "by-wwn" || b.TargetDisk.Value != "0x500a075119abcdef" {
		t.Fatalf("TargetDisk after set: %+v", b.TargetDisk)
	}

	// 3) Upsert without TargetDisk in input → keep existing.
	b, err = f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "reinstall", UpdatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("Upsert 3 (keep): %v", err)
	}
	if b.TargetDisk == nil || b.TargetDisk.Mode != "by-wwn" {
		t.Fatalf("TargetDisk after keep upsert: %+v", b.TargetDisk)
	}

	// 4) Explicit clear via JSON null.
	b, err = f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		TargetDisk: []byte("null"),
	})
	if err != nil {
		t.Fatalf("Upsert 4 (clear via null): %v", err)
	}
	if b.TargetDisk != nil {
		t.Fatalf("TargetDisk after clear-via-null: got %+v, want nil", b.TargetDisk)
	}

	// 5) Set again, then clear via empty object — same semantics.
	if _, err := f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		TargetDisk: tdRaw,
	}); err != nil {
		t.Fatalf("Upsert 5a (set): %v", err)
	}
	b, err = f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		TargetDisk: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("Upsert 5b (clear via {}): %v", err)
	}
	if b.TargetDisk != nil {
		t.Fatalf("TargetDisk after clear-via-empty-object: got %+v, want nil", b.TargetDisk)
	}

	// 6) Invalid target_disk JSON → validation error, row unchanged.
	bad := []byte(`{"mode":"bogus"}`)
	if _, err := f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		TargetDisk: bad,
	}); err == nil || !strings.Contains(err.Error(), "target_disk") {
		t.Fatalf("want target_disk validation error, got %v", err)
	}

	// Get sanity: row still resolves and override still cleared.
	got, err := f.bindings.Get(ctx, mu)
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	if got.TargetDisk != nil {
		t.Fatalf("final TargetDisk: got %+v, want nil", got.TargetDisk)
	}
}

// TestBondOverrideThreeState exercises the per-binding bond override knob
// across the same three states as TestTargetDiskOverrideThreeState. We use
// active-backup with two MAC slaves as the representative valid value.
func TestBondOverrideThreeState(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '0')
	im := f.seedImage(t, "1")
	pr := f.seedProfile(t, "p-bond", "dhcp")
	ctx := context.Background()

	// 1) First upsert without bond override → binding.Bond == nil.
	b, err := f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("Upsert 1: %v", err)
	}
	if b.Bond != nil {
		t.Fatalf("Bond after no-override upsert: got %+v, want nil", b.Bond)
	}

	// 2) Set a bond override (active-backup, two slaves).
	bondRaw, _ := json.Marshal(profiles.BondConfig{
		Mode:   "active-backup",
		Slaves: []string{"eno1", "eno2"},
	})
	b, err = f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		Bond: bondRaw,
	})
	if err != nil {
		t.Fatalf("Upsert 2 (set bond): %v", err)
	}
	if b.Bond == nil || b.Bond.Mode != "active-backup" || len(b.Bond.Slaves) != 2 {
		t.Fatalf("Bond after set: %+v", b.Bond)
	}

	// 3) Upsert without Bond field → keep existing.
	b, err = f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "reinstall", UpdatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("Upsert 3 (keep): %v", err)
	}
	if b.Bond == nil || b.Bond.Mode != "active-backup" {
		t.Fatalf("Bond after keep upsert: %+v", b.Bond)
	}

	// 4) Explicit clear via JSON null.
	b, err = f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		Bond: []byte("null"),
	})
	if err != nil {
		t.Fatalf("Upsert 4 (clear via null): %v", err)
	}
	if b.Bond != nil {
		t.Fatalf("Bond after clear-via-null: got %+v, want nil", b.Bond)
	}

	// 5) Invalid bond JSON → validation error, row unchanged.
	bad := []byte(`{"mode":"bogus","slaves":["eno1","eno2"]}`)
	if _, err := f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		Bond: bad,
	}); err == nil || !strings.Contains(err.Error(), "bond") {
		t.Fatalf("want bond validation error, got %v", err)
	}

	// 6) Only one slave → validation error.
	tooFew := []byte(`{"mode":"active-backup","slaves":["eno1"]}`)
	if _, err := f.bindings.Upsert(ctx, UpsertInput{
		MachineUUID: mu, ImageID: im, ProfileID: pr,
		DesiredState: "install", UpdatedBy: "admin",
		Bond: tooFew,
	}); err == nil || !strings.Contains(err.Error(), "slaves") {
		t.Fatalf("want bond.slaves validation error, got %v", err)
	}
}
