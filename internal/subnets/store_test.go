package subnets

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"metalkit/internal/sqlitedb"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{Path: path, Logger: logger})
	if err != nil {
		t.Fatalf("sqlitedb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := NewStore(context.Background(), db, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func validInput(name string) CreateInput {
	return CreateInput{
		Name:        name,
		Description: "test subnet",
		CIDR:        "192.168.10.0/24",
		Gateway:     "192.168.10.1",
		DNS:         []string{"1.1.1.1", "8.8.8.8"},
		VLANID:      0,
		CreatedBy:   "admin",
	}
}

func TestCreateGet(t *testing.T) {
	s := newTestStore(t)
	in := validInput("net-a")
	sn, err := s.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !subnetIDRE.MatchString(sn.ID) {
		t.Errorf("id %q: not 32 hex", sn.ID)
	}
	if sn.Name != "net-a" || sn.CIDR != "192.168.10.0/24" {
		t.Errorf("fields not echoed: %+v", sn)
	}
	if sn.Gateway != "192.168.10.1" {
		t.Errorf("gateway = %q", sn.Gateway)
	}
	if len(sn.DNS) != 2 {
		t.Errorf("dns len = %d, want 2", len(sn.DNS))
	}

	got, err := s.Get(context.Background(), sn.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != sn.Name || got.CIDR != sn.CIDR || got.Gateway != sn.Gateway {
		t.Errorf("roundtrip mismatch: created=%+v got=%+v", sn, got)
	}
}

func TestCreateCIDRCanonicalised(t *testing.T) {
	s := newTestStore(t)
	in := validInput("canon")
	in.CIDR = "192.168.10.50/24" // host bits set; should mask down
	sn, err := s.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sn.CIDR != "192.168.10.0/24" {
		t.Errorf("cidr = %q, want canonicalised 192.168.10.0/24", sn.CIDR)
	}
}

func TestCreateDuplicate(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Create(context.Background(), validInput("dup")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.Create(context.Background(), validInput("dup"))
	if !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("second create: got %v, want ErrDuplicateName", err)
	}
}

func TestCreateRejectsBadFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cases := []struct {
		name   string
		mut    func(*CreateInput)
		errSub string
	}{
		{"empty name", func(in *CreateInput) { in.Name = "" }, "name"},
		{"bad name chars", func(in *CreateInput) { in.Name = "has space" }, "name"},
		{"long name", func(in *CreateInput) { in.Name = strings.Repeat("a", 100) }, "name"},
		{"bad cidr", func(in *CreateInput) { in.CIDR = "not-a-cidr" }, "cidr"},
		{"ipv6 cidr", func(in *CreateInput) { in.CIDR = "fe80::/64" }, "IPv4 only"},
		{"gateway outside cidr", func(in *CreateInput) {
			in.Gateway = "10.0.0.1"
		}, "not inside cidr"},
		{"gateway is network", func(in *CreateInput) {
			in.Gateway = "192.168.10.0"
		}, "network"},
		{"gateway is broadcast", func(in *CreateInput) {
			in.Gateway = "192.168.10.255"
		}, "broadcast"},
		{"bad dns", func(in *CreateInput) {
			in.DNS = []string{"not.an.ip"}
		}, "dns"},
		{"bad vlan", func(in *CreateInput) {
			in.VLANID = 9999
		}, "vlan_id"},
		{"missing created_by", func(in *CreateInput) {
			in.CreatedBy = ""
		}, "created_by"},
		{"long description", func(in *CreateInput) {
			in.Description = strings.Repeat("x", MaxDescriptionLen+1)
		}, "description"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validInput("ok-" + strings.ReplaceAll(c.name, " ", "-"))
			c.mut(&in)
			_, err := s.Create(ctx, in)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.errSub)
			}
			if !strings.Contains(err.Error(), c.errSub) {
				t.Errorf("error %q does not contain %q", err.Error(), c.errSub)
			}
		})
	}
}

func TestListOrdersNewestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, n := range []string{"a", "b", "c"} {
		if _, err := s.Create(ctx, validInput(n)); err != nil {
			t.Fatalf("Create %s: %v", n, err)
		}
	}
	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got)=%d, want 3", len(got))
	}
	seen := map[string]int{}
	for _, sn := range got {
		seen[sn.Name]++
	}
	for _, n := range []string{"a", "b", "c"} {
		if seen[n] != 1 {
			t.Errorf("name %q: count=%d, want 1", n, seen[n])
		}
	}
}

func TestGetByName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sn, err := s.Create(ctx, validInput("named"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.GetByName(ctx, "named")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.ID != sn.ID {
		t.Errorf("id mismatch: got %s want %s", got.ID, sn.ID)
	}
	if _, err := s.GetByName(ctx, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing name: got %v, want ErrNotFound", err)
	}
}

func TestUpdatePartial(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sn, err := s.Create(ctx, validInput("u1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orig := *sn

	newDesc := "updated desc"
	got, err := s.Update(ctx, sn.ID, UpdateInput{Description: &newDesc, UpdatedBy: "bob"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Description != newDesc {
		t.Errorf("description = %q, want %q", got.Description, newDesc)
	}
	if got.CIDR != orig.CIDR || got.Gateway != orig.Gateway {
		t.Errorf("unrelated fields changed: orig=%+v got=%+v", orig, got)
	}
	if got.UpdatedBy != "bob" {
		t.Errorf("updated_by = %q, want bob", got.UpdatedBy)
	}
}

func TestUpdateCIDRReValidatesGateway(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sn, err := s.Create(ctx, validInput("u2"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Change CIDR to a range that doesn't contain the existing gateway.
	newCIDR := "10.0.0.0/24"
	_, err = s.Update(ctx, sn.ID, UpdateInput{CIDR: &newCIDR})
	if err == nil || !strings.Contains(err.Error(), "not inside cidr") {
		t.Errorf("expected gateway-outside-cidr error, got %v", err)
	}
	// Now move both at once: should succeed.
	newGW := "10.0.0.1"
	got, err := s.Update(ctx, sn.ID, UpdateInput{CIDR: &newCIDR, Gateway: &newGW})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.CIDR != newCIDR || got.Gateway != newGW {
		t.Errorf("update did not stick: %+v", got)
	}
}

func TestUpdateNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Update(context.Background(), "00000000000000000000000000000000", UpdateInput{})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Update on missing id: got %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sn, err := s.Create(ctx, validInput("d1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete(ctx, sn.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = s.Get(ctx, sn.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: got %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, sn.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete: got %v, want ErrNotFound", err)
	}
}
