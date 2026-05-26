package bmc

import (
	"context"
	"errors"
	"testing"
)

func TestPlaceholderUUID(t *testing.T) {
	cases := []struct {
		ip   string
		want string
	}{
		{"192.168.10.254", "placeholder-192-168-10-254"},
		{"10.0.0.1", "placeholder-10-0-0-1"},
		{"  10.0.0.1  ", "placeholder-10-0-0-1"},
		{"not-an-ip", ""},
		{"", ""},
		{"::1", ""},                  // IPv6 not supported
		{"2001:db8::1", ""},
	}
	for _, c := range cases {
		got := PlaceholderUUID(c.ip)
		if got != c.want {
			t.Errorf("PlaceholderUUID(%q)=%q want %q", c.ip, got, c.want)
		}
	}
}

func TestIsPlaceholderUUID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"placeholder-192-168-10-254", true},
		{"placeholder-", true}, // technically true; validatePlaceholderUUID rejects it
		{"4c4c4544-0058-3210-8053-c5c04f463830", false},
		{"", false},
	}
	for _, c := range cases {
		got := IsPlaceholderUUID(c.in)
		if got != c.want {
			t.Errorf("IsPlaceholderUUID(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestValidatePlaceholderUUID(t *testing.T) {
	ok, err := validatePlaceholderUUID("PLACEHOLDER-192-168-10-254")
	if err != nil {
		t.Fatalf("uppercase: %v", err)
	}
	if ok != "placeholder-192-168-10-254" {
		t.Errorf("normalize: got %q", ok)
	}

	for _, bad := range []string{
		"4c4c4544-0058-3210-8053-c5c04f463830", // SMBIOS form
		"placeholder-",
		"placeholder-not-an-ip",
		"placeholder-999-999-999-999",
		"",
	} {
		if _, err := validatePlaceholderUUID(bad); err == nil {
			t.Errorf("validatePlaceholderUUID(%q) accepted; want error", bad)
		}
	}
}

func TestUpsertCreatesPlaceholderMachine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	placeholder := PlaceholderUUID("192.168.10.254")
	c, err := f.store.Upsert(ctx, UpsertInput{
		MachineUUID: placeholder,
		IP:          "192.168.10.254",
		Username:    "root",
		Password:    pstr("calvin"),
		UpdatedBy:   "admin",
	})
	if err != nil {
		t.Fatalf("Upsert placeholder: %v", err)
	}
	if c.MachineUUID != placeholder {
		t.Errorf("MachineUUID=%q want %q", c.MachineUUID, placeholder)
	}

	// Machine row should have been auto-created with status='unknown'.
	var status string
	if err := f.db.QueryRowContext(ctx,
		`SELECT status FROM machines WHERE uuid = ?`, placeholder).Scan(&status); err != nil {
		t.Fatalf("query machine: %v", err)
	}
	if status != "unknown" {
		t.Errorf("placeholder status=%q want 'unknown'", status)
	}
}

func TestUpsertPlaceholderIPMismatch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Placeholder UUID encodes 192.168.10.254 but the body claims 10.0.0.1.
	_, err := f.store.Upsert(ctx, UpsertInput{
		MachineUUID: PlaceholderUUID("192.168.10.254"),
		IP:          "10.0.0.1",
		Username:    "root",
		Password:    pstr("calvin"),
		UpdatedBy:   "admin",
	})
	if err == nil {
		t.Fatalf("want IP mismatch error, got nil")
	}
}

func TestDeletePlaceholderCleansUpMachine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	placeholder := PlaceholderUUID("10.20.30.40")
	if _, err := f.store.Upsert(ctx, UpsertInput{
		MachineUUID: placeholder,
		IP:          "10.20.30.40",
		Username:    "root",
		Password:    pstr("calvin"),
		UpdatedBy:   "admin",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := f.store.Delete(ctx, placeholder); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var seen int
	err := f.db.QueryRowContext(ctx,
		`SELECT 1 FROM machines WHERE uuid = ?`, placeholder).Scan(&seen)
	if err == nil {
		t.Errorf("placeholder machine still present after BMC delete")
	}
}

func TestFindByIP(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mu := f.seedMachine(t, 'b')
	if _, err := f.store.Upsert(ctx, UpsertInput{
		MachineUUID: mu,
		IP:          "10.0.0.42",
		Username:    "u",
		Password:    pstr("p"),
		UpdatedBy:   "admin",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := f.store.FindByIP(ctx, "10.0.0.42")
	if err != nil {
		t.Fatalf("FindByIP hit: %v", err)
	}
	if got != mu {
		t.Errorf("FindByIP hit=%q want %q", got, mu)
	}

	if _, err := f.store.FindByIP(ctx, "10.0.0.99"); !errors.Is(err, ErrNotFound) {
		t.Errorf("FindByIP miss=%v want ErrNotFound", err)
	}
}

func TestReconcilePlaceholder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// 1. Operator registers BMC by IP before the host PXE'd.
	placeholder := PlaceholderUUID("172.16.0.50")
	if _, err := f.store.Upsert(ctx, UpsertInput{
		MachineUUID: placeholder,
		IP:          "172.16.0.50",
		Username:    "root",
		Password:    pstr("calvin"),
		UpdatedBy:   "admin",
	}); err != nil {
		t.Fatalf("seed placeholder: %v", err)
	}

	// 2. Real machine reports — simulate the inventory caller by inserting a
	// machine row with a SMBIOS UUID. (In production this is the upsert tx.)
	real := f.seedMachine(t, 'c')

	// 3. Reconcile.
	if err := f.store.ReconcilePlaceholder(ctx, real, "172.16.0.50"); err != nil {
		t.Fatalf("ReconcilePlaceholder: %v", err)
	}

	// 4. Credential is now owned by the real UUID.
	c, err := f.store.Get(ctx, real)
	if err != nil {
		t.Fatalf("Get real: %v", err)
	}
	if c.IP != "172.16.0.50" {
		t.Errorf("IP=%q want 172.16.0.50", c.IP)
	}

	// 5. Placeholder rows are gone.
	if _, err := f.store.Get(ctx, placeholder); !errors.Is(err, ErrNotFound) {
		t.Errorf("placeholder credential still present: %v", err)
	}
	var seen int
	if err := f.db.QueryRowContext(ctx,
		`SELECT 1 FROM machines WHERE uuid = ?`, placeholder).Scan(&seen); err == nil {
		t.Errorf("placeholder machine row still present")
	}
}

func TestReconcilePlaceholderNoOpWhenNoPlaceholder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	real := f.seedMachine(t, 'd')
	// No placeholder exists for this IP — call should succeed and do nothing.
	if err := f.store.ReconcilePlaceholder(ctx, real, "172.16.99.99"); err != nil {
		t.Fatalf("no-op reconcile: %v", err)
	}
}

func TestReconcilePlaceholderRejectsNonSMBIOS(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.store.ReconcilePlaceholder(ctx, "not-a-uuid", "10.0.0.1"); err == nil {
		t.Fatalf("want error for non-SMBIOS real uuid, got nil")
	}
}
