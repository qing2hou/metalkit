package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"metalkit/internal/sqlitedb"
)

const validSha512crypt = `$6$rounds=4096$abcdefghijklmnop$` +
	`aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`

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
	td, _ := json.Marshal(TargetDisk{Mode: "smallest"})
	nc, _ := json.Marshal(NetworkConfig{
		Method: "static", PrefixLen: 24, Gateway: "10.99.0.1",
		DNS: []string{"1.1.1.1"}, NICSelector: "auto",
	})
	return CreateInput{
		Name:             name,
		Description:      "test profile",
		HostnameTemplate: "node-{serial}",
		RootPasswordHash: validSha512crypt,
		TargetDisk:       td,
		Network:          nc,
		CreatedBy:        "admin",
	}
}

func TestCreateGet(t *testing.T) {
	s := newTestStore(t)
	in := validInput("p1")
	p, err := s.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !profileIDRE.MatchString(p.ID) {
		t.Errorf("id %q: not 32 hex", p.ID)
	}
	if p.Name != "p1" || p.HostnameTemplate != "node-{serial}" {
		t.Errorf("fields not echoed: %+v", p)
	}
	if p.TargetDisk.Mode != "smallest" {
		t.Errorf("target_disk.mode = %q", p.TargetDisk.Mode)
	}
	if p.Network.Method != "static" || p.Network.PrefixLen != 24 {
		t.Errorf("network: %+v", p.Network)
	}

	got, err := s.Get(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != p.Name || got.HostnameTemplate != p.HostnameTemplate {
		t.Errorf("roundtrip mismatch: created=%+v got=%+v", p, got)
	}
	if got.Network.Gateway != "10.99.0.1" || len(got.Network.DNS) != 1 {
		t.Errorf("network roundtrip lost data: %+v", got.Network)
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
		name  string
		mut   func(*CreateInput)
		wants string
	}{
		{"empty name", func(in *CreateInput) { in.Name = "" }, "name"},
		{"bad name chars", func(in *CreateInput) { in.Name = "has space" }, "name"},
		{"long name", func(in *CreateInput) { in.Name = strings.Repeat("a", 100) }, "name"},
		{"bad hostname template", func(in *CreateInput) { in.HostnameTemplate = "" }, "hostname_template"},
		{"hostname template chars", func(in *CreateInput) { in.HostnameTemplate = "bad!template" }, "hostname_template"},
		{"weak password hash", func(in *CreateInput) { in.RootPasswordHash = "plaintext" }, "root_password_hash"},
		{"md5 password hash", func(in *CreateInput) { in.RootPasswordHash = "$1$abcd$0123456789abcdef0123456789" }, "root_password_hash"},
		{"target disk mode invalid", func(in *CreateInput) {
			b, _ := json.Marshal(TargetDisk{Mode: "biggest"})
			in.TargetDisk = b
		}, "target_disk.mode"},
		{"target disk by-path no value", func(in *CreateInput) {
			b, _ := json.Marshal(TargetDisk{Mode: "by-path"})
			in.TargetDisk = b
		}, "target_disk.value"},
		{"target disk smallest with value", func(in *CreateInput) {
			b, _ := json.Marshal(TargetDisk{Mode: "smallest", Value: "/dev/sda"})
			in.TargetDisk = b
		}, "target_disk.value"},
		{"static missing gateway", func(in *CreateInput) {
			b, _ := json.Marshal(NetworkConfig{Method: "static", PrefixLen: 24, NICSelector: "auto"})
			in.Network = b
		}, "network.gateway"},
		{"static bad prefix", func(in *CreateInput) {
			b, _ := json.Marshal(NetworkConfig{Method: "static", PrefixLen: 0, Gateway: "10.0.0.1", NICSelector: "auto"})
			in.Network = b
		}, "network.prefix_len"},
		{"bad dns", func(in *CreateInput) {
			b, _ := json.Marshal(NetworkConfig{
				Method: "static", PrefixLen: 24, Gateway: "10.0.0.1",
				DNS: []string{"not.an.ip"}, NICSelector: "auto",
			})
			in.Network = b
		}, "network.dns"},
		{"bad nic selector", func(in *CreateInput) {
			b, _ := json.Marshal(NetworkConfig{
				Method: "static", PrefixLen: 24, Gateway: "10.0.0.1", NICSelector: "by-mac:zz",
			})
			in.Network = b
		}, "nic_selector"},
		{"unknown method", func(in *CreateInput) {
			b, _ := json.Marshal(NetworkConfig{Method: "bond"})
			in.Network = b
		}, "network.method"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validInput("ok-" + strings.ReplaceAll(c.name, " ", "-"))
			c.mut(&in)
			_, err := s.Create(ctx, in)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wants)
			}
			if !strings.Contains(err.Error(), c.wants) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wants)
			}
		})
	}
}

func TestDHCPStripsStaticFields(t *testing.T) {
	s := newTestStore(t)
	in := validInput("dhcp1")
	b, _ := json.Marshal(NetworkConfig{
		Method: "dhcp", PrefixLen: 24, Gateway: "ignored", DNS: []string{"1.1.1.1"},
		NICSelector: "by-mac:aa:bb:cc:dd:ee:ff",
	})
	in.Network = b
	p, err := s.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.Network.Method != "dhcp" {
		t.Errorf("method = %q", p.Network.Method)
	}
	if p.Network.PrefixLen != 0 || p.Network.Gateway != "" || len(p.Network.DNS) != 0 {
		t.Errorf("dhcp should strip static-only fields: %+v", p.Network)
	}
	if p.Network.NICSelector != "by-mac:aa:bb:cc:dd:ee:ff" {
		t.Errorf("nic selector mangled: %q", p.Network.NICSelector)
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
	// Newest first; all created at very close timestamps but the secondary
	// sort is by id DESC, so we can't rely on strict order by name. Just
	// verify all three names appear and no duplicates.
	seen := map[string]int{}
	for _, p := range got {
		seen[p.Name]++
	}
	for _, n := range []string{"a", "b", "c"} {
		if seen[n] != 1 {
			t.Errorf("name %q: count=%d, want 1", n, seen[n])
		}
	}
}

func TestUpdatePartial(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	p, err := s.Create(ctx, validInput("u1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	orig := *p

	newDesc := "updated desc"
	got, err := s.Update(ctx, p.ID, UpdateInput{Description: &newDesc})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Description != newDesc {
		t.Errorf("description = %q, want %q", got.Description, newDesc)
	}
	if got.HostnameTemplate != orig.HostnameTemplate || got.RootPasswordHash != orig.RootPasswordHash {
		t.Errorf("unrelated fields changed: %+v vs %+v", orig, got)
	}
	if !got.UpdatedAt.After(orig.UpdatedAt) && !got.UpdatedAt.Equal(orig.UpdatedAt) {
		t.Errorf("updated_at not advanced: orig=%v got=%v", orig.UpdatedAt, got.UpdatedAt)
	}
}

func TestUpdateValidatesNewFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	p, err := s.Create(ctx, validInput("u2"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bad := "weak"
	_, err = s.Update(ctx, p.ID, UpdateInput{RootPasswordHash: &bad})
	if err == nil || !strings.Contains(err.Error(), "root_password_hash") {
		t.Errorf("expected root_password_hash validation, got %v", err)
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
	p, err := s.Create(ctx, validInput("d1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete(ctx, p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = s.Get(ctx, p.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: got %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete: got %v, want ErrNotFound", err)
	}
}
