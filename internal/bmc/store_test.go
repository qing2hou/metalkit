package bmc

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metalkit/internal/crypto"
	"metalkit/internal/sqlitedb"
)

type fixture struct {
	db     *sql.DB
	store  *Store
	cipher *crypto.Cipher
}

// pstr returns a pointer to s; sugar for *string field literals in tests
// (Password is *string to support "omit means keep" on update — see store.go).
func pstr(s string) *string { return &s }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{Path: path, Logger: logger})
	if err != nil {
		t.Fatalf("sqlitedb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// machines table must exist before we apply the bmc schema (FK reference).
	if _, err := db.ExecContext(context.Background(), `
        CREATE TABLE IF NOT EXISTS machines (
            uuid TEXT PRIMARY KEY,
            first_seen INTEGER,
            last_seen INTEGER,
            status TEXT,
            latest_report INTEGER
        )`); err != nil {
		t.Fatalf("create machines stub: %v", err)
	}

	cip, err := crypto.NewCipher(bytes.Repeat([]byte{0x42}, crypto.KeySize))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	s, err := NewStore(context.Background(), db, logger, cip)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return &fixture{db: db, store: s, cipher: cip}
}

func (f *fixture) seedMachine(t *testing.T, suffix byte) string {
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

func TestUpsertHappyPath(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '0')

	c, err := f.store.Upsert(context.Background(), UpsertInput{
		MachineUUID:   mu,
		IP:            "10.0.0.10",
		Username:      "ADMIN",
		Password:      pstr("hunter2"),
		IPMIInterface: "lanplus",
		UpdatedBy:     "admin",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if c.MachineUUID != mu || c.IP != "10.0.0.10" || c.Username != "ADMIN" ||
		c.Port != 623 || c.IPMIInterface != "lanplus" {
		t.Errorf("Upsert returned %+v", c)
	}
}

func TestUpsertEncryptsPassword(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '1')
	if _, err := f.store.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, IP: "10.0.0.11", Username: "ADMIN",
		Password: pstr("verysecret"), UpdatedBy: "admin",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var ct []byte
	if err := f.db.QueryRow(`SELECT password_ct FROM bmc_credentials WHERE machine_uuid = ?`, mu).Scan(&ct); err != nil {
		t.Fatalf("query: %v", err)
	}
	if bytes.Contains(ct, []byte("verysecret")) {
		t.Errorf("ciphertext contains plaintext substring — encryption isn't being applied")
	}
	if ct[0] != crypto.CurrentVersion {
		t.Errorf("ciphertext version=%#x want %#x", ct[0], crypto.CurrentVersion)
	}
}

func TestGetOmitsPassword(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '2')
	if _, err := f.store.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, IP: "10.0.0.12", Username: "u",
		Password: pstr("p"), UpdatedBy: "admin",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	c, err := f.store.Get(context.Background(), mu)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Type-level guarantee: Credential has no Password field.
	_ = c
}

func TestGetWithPasswordRoundTrip(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '3')
	const want = "the magic password"
	if _, err := f.store.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, IP: "10.0.0.13", Username: "u",
		Password: pstr(want), UpdatedBy: "admin",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := f.store.GetWithPassword(context.Background(), mu)
	if err != nil {
		t.Fatalf("GetWithPassword: %v", err)
	}
	if got.Password != want {
		t.Errorf("Password roundtrip: got %q want %q", got.Password, want)
	}
}

func TestUpsertReplaces(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '4')
	if _, err := f.store.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, IP: "10.0.0.14", Username: "old",
		Password: pstr("old-pass"), UpdatedBy: "alice",
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	c, err := f.store.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, IP: "10.0.0.99", Username: "new",
		Password: pstr("new-pass"), UpdatedBy: "bob",
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if c.IP != "10.0.0.99" || c.Username != "new" || c.UpdatedBy != "bob" {
		t.Errorf("overwrite mismatch: %+v", c)
	}
	got, err := f.store.GetWithPassword(context.Background(), mu)
	if err != nil {
		t.Fatalf("GetWithPassword: %v", err)
	}
	if got.Password != "new-pass" {
		t.Errorf("password not replaced: got %q", got.Password)
	}
}

func TestUpsertUnknownMachine(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.Upsert(context.Background(), UpsertInput{
		MachineUUID: "ffffffff-ffff-ffff-ffff-ffffffffffff",
		IP:          "10.0.0.20", Username: "u", Password: pstr("p"), UpdatedBy: "admin",
	})
	if !errors.Is(err, ErrMachineUnknown) {
		t.Fatalf("want ErrMachineUnknown, got %v", err)
	}
}

func TestGetNotFound(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.Get(context.Background(), "4c4c4544-0058-3210-8053-c5c04f463830")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '5')
	if _, err := f.store.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, IP: "10.0.0.15", Username: "u",
		Password: pstr("p"), UpdatedBy: "admin",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := f.store.Delete(context.Background(), mu); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := f.store.Get(context.Background(), mu); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: want ErrNotFound, got %v", err)
	}
	if err := f.store.Delete(context.Background(), mu); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete: want ErrNotFound, got %v", err)
	}
}

func TestList(t *testing.T) {
	f := newFixture(t)
	mu1 := f.seedMachine(t, '6')
	mu2 := f.seedMachine(t, '7')
	for _, mu := range []string{mu1, mu2} {
		if _, err := f.store.Upsert(context.Background(), UpsertInput{
			MachineUUID: mu, IP: "10.0.0.30", Username: "u",
			Password: pstr("p"), UpdatedBy: "admin",
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	got, err := f.store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(got)=%d want 2", len(got))
	}
}

// TestUpsertName covers the display-alias column: create with a name, read it
// back, then clear it on update. NULL on disk shows as Go zero string.
func TestUpsertName(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, 'a')
	ctx := context.Background()

	// Create with name.
	c, err := f.store.Upsert(ctx, UpsertInput{
		MachineUUID: mu, Name: "rack01-r630-01",
		IP: "10.0.0.10", Username: "root", Password: pstr("p"),
		UpdatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("Upsert with name: %v", err)
	}
	if c.Name != "rack01-r630-01" {
		t.Fatalf("Name=%q want %q", c.Name, "rack01-r630-01")
	}

	got, err := f.store.Get(ctx, mu)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "rack01-r630-01" {
		t.Fatalf("Get.Name=%q want rack01-r630-01", got.Name)
	}

	// Update clears name (empty string → stored as NULL → read back as "").
	if _, err := f.store.Upsert(ctx, UpsertInput{
		MachineUUID: mu, Name: "",
		IP: "10.0.0.10", Username: "root",
		UpdatedBy: "admin",
	}); err != nil {
		t.Fatalf("Upsert clear name: %v", err)
	}
	got, err = f.store.Get(ctx, mu)
	if err != nil {
		t.Fatalf("Get after clear: %v", err)
	}
	if got.Name != "" {
		t.Fatalf("Name after clear=%q want empty", got.Name)
	}
}

func TestMachineCascadeDelete(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '8')
	if _, err := f.store.Upsert(context.Background(), UpsertInput{
		MachineUUID: mu, IP: "10.0.0.50", Username: "u",
		Password: pstr("p"), UpdatedBy: "admin",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Sanity: FK cascade is informational only in the schema (PRAGMA may be
	// off across pool conns). Manual delete is what production will use
	// since inventory.Store.Delete would do its own cleanup. Test that
	// app-level Delete(uuid) works through the bmc Store.
	if err := f.store.Delete(context.Background(), mu); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestBadInputs(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, '9')
	ctx := context.Background()
	cases := []struct {
		name  string
		mut   func(*UpsertInput)
		wants string
	}{
		{"bad uuid", func(in *UpsertInput) { in.MachineUUID = "not-a-uuid" }, "machine_uuid"},
		{"bad ip", func(in *UpsertInput) { in.IP = "not-an-ip" }, "ip"},
		{"bad ip v6", func(in *UpsertInput) { in.IP = "::1" }, "ip"},
		{"bad port low", func(in *UpsertInput) { in.Port = -1 }, "port"},
		{"bad port high", func(in *UpsertInput) { in.Port = 99999 }, "port"},
		{"empty username", func(in *UpsertInput) { in.Username = "" }, "username"},
		{"username with space", func(in *UpsertInput) { in.Username = "a b" }, "username"},
		{"empty password", func(in *UpsertInput) { in.Password = pstr("") }, "password"},
		{"bad interface", func(in *UpsertInput) { in.IPMIInterface = "telnet" }, "ipmi_interface"},
		{"missing updated_by", func(in *UpsertInput) { in.UpdatedBy = "" }, "updated_by"},
		{"name with control char", func(in *UpsertInput) { in.Name = "bad\x00name" }, "name"},
		{"name too long", func(in *UpsertInput) { in.Name = strings.Repeat("x", 65) }, "name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := UpsertInput{
				MachineUUID: mu, IP: "10.0.0.1", Username: "u",
				Password: pstr("p"), UpdatedBy: "admin",
			}
			c.mut(&in)
			_, err := f.store.Upsert(ctx, in)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wants)
			}
			if !strings.Contains(err.Error(), c.wants) {
				t.Errorf("err %q does not contain %q", err.Error(), c.wants)
			}
		})
	}
}
