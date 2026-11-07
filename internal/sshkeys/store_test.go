package sshkeys

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
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
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{Path: path, Logger: logger, NoSync: true})
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

// Real throwaway key generated with `ssh-keygen -t ed25519` — its
// fingerprint below was taken from `ssh-keygen -lf` so the parser is pinned
// to OpenSSH's fingerprint format, not to its own implementation.
const (
	testEd25519Pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIEeIM72OY68FIhL6i3KsXruAYy0C5matj1jacX/rDwd qa-test@metalkit"
	testEd25519FP  = "SHA256:63qVzQBaU1D9wjJBXscCVL1+tS1u8yQaR3UjyRH8wYw"
)

func validCreate(name string) CreateInput {
	return CreateInput{Name: name, PublicKey: testEd25519Pub, CreatedBy: "admin"}
}

func TestCreateGetRoundtrip(t *testing.T) {
	s := newTestStore(t)
	k, err := s.Create(context.Background(), validCreate("ops-key"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !sshKeyIDRE.MatchString(k.ID) {
		t.Errorf("id %q: not 32 hex", k.ID)
	}
	if k.KeyType != "ssh-ed25519" {
		t.Errorf("key_type = %q, want ssh-ed25519", k.KeyType)
	}
	if k.Fingerprint != testEd25519FP {
		t.Errorf("fingerprint = %q, want %q (ssh-keygen -lf)", k.Fingerprint, testEd25519FP)
	}
	if k.Comment != "qa-test@metalkit" {
		t.Errorf("comment = %q", k.Comment)
	}
	if k.PublicKey != testEd25519Pub {
		t.Errorf("public_key = %q, want canonical roundtrip", k.PublicKey)
	}

	got, err := s.Get(context.Background(), k.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "ops-key" || got.Fingerprint != testEd25519FP {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

func TestCreateDuplicateName(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Create(context.Background(), validCreate("dup")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := s.Create(context.Background(), validCreate("dup"))
	if !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("second Create err = %v, want ErrDuplicateName", err)
	}
}

func TestCreateRejectsInvalidKeys(t *testing.T) {
	s := newTestStore(t)
	cases := []struct {
		name string
		key  string
		want string
	}{
		{"empty", "", "不能为空"},
		{"private-key-paste", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END-----", "单行"},
		{"two-lines", testEd25519Pub + "\n" + testEd25519Pub, "单行"},
		{"type-only", "ssh-ed25519", "base64"},
		{"unknown-type", "ssh-fancy " + strings.Fields(testEd25519Pub)[1] + " c", "类型"},
		{"bad-base64", "ssh-ed25519 !!!not-base64!!! c", "base64"},
		{"type-mismatch",
			"ssh-rsa " + strings.Fields(testEd25519Pub)[1] + " c", "不匹配"},
	}
	for _, tc := range cases {
		in := validCreate("k-" + tc.name)
		in.PublicKey = tc.key
		_, err := s.Create(context.Background(), in)
		if err == nil {
			t.Errorf("%s: Create succeeded, want error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %q, want substring %q", tc.name, err, tc.want)
		}
	}
}

func TestCreateCanonicalisesWhitespace(t *testing.T) {
	s := newTestStore(t)
	in := validCreate("ws")
	// 注释多余空格 + 首尾空白：规范化后应与规范形式一致
	fields := strings.Fields(testEd25519Pub)
	in.PublicKey = "  " + fields[0] + "   " + fields[1] + "  qa-test@metalkit \n"
	k, err := s.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if k.PublicKey != testEd25519Pub {
		t.Errorf("public_key = %q, want %q", k.PublicKey, testEd25519Pub)
	}
}

func TestCreateNameValidation(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"", strings.Repeat("a", 65), "a b"} {
		in := validCreate(name)
		if _, err := s.Create(context.Background(), in); err == nil {
			t.Errorf("name %q: Create succeeded, want error", name)
		}
	}
	// 中文/点/下划线/连字符合法（与 subnets 命名规则一致）；
	// 首尾空白被规范化后接受（validateName TrimSpace）
	for _, name := range []string{"运维主密钥", "ops.key_1-x", "  leadspace-ok  "} {
		in := validCreate(name)
		if _, err := s.Create(context.Background(), in); err != nil {
			t.Errorf("name %q: Create: %v", name, err)
		}
	}
}

func TestUpdateThreeState(t *testing.T) {
	s := newTestStore(t)
	k, err := s.Create(context.Background(), validCreate("old"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// nil 字段 = 全保持
	got, err := s.Update(context.Background(), k.ID, UpdateInput{})
	if err != nil {
		t.Fatalf("Update no-op: %v", err)
	}
	if got.Name != "old" || got.PublicKey != testEd25519Pub {
		t.Errorf("no-op changed fields: %+v", got)
	}

	// 只改名
	newName := "renamed"
	got, err = s.Update(context.Background(), k.ID, UpdateInput{Name: &newName})
	if err != nil {
		t.Fatalf("Update rename: %v", err)
	}
	if got.Name != "renamed" || got.Fingerprint != testEd25519FP {
		t.Errorf("rename lost fields: %+v", got)
	}

	// 改公钥 → 派生字段跟随
	rsa := makeTestKeyLine(t, "ssh-rsa")
	got, err = s.Update(context.Background(), k.ID, UpdateInput{PublicKey: &rsa.line})
	if err != nil {
		t.Fatalf("Update rekey: %v", err)
	}
	if got.KeyType != "ssh-rsa" || got.Fingerprint != rsa.fingerprint {
		t.Errorf("rekey derived fields wrong: %q / %q", got.KeyType, got.Fingerprint)
	}
	if got.Name != "renamed" {
		t.Errorf("rekey clobbered name: %q", got.Name)
	}

	// 重名拒绝：把第一个 key 改成 other 已占用的名字
	other, err := s.Create(context.Background(), validCreate("taken"))
	if err != nil {
		t.Fatalf("Create other: %v", err)
	}
	_ = other
	n2 := "taken"
	if _, err := s.Update(context.Background(), k.ID, UpdateInput{Name: &n2}); err == nil {
		t.Errorf("rename to taken name succeeded, want error")
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)
	k, err := s.Create(context.Background(), validCreate("gone"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete(context.Background(), k.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(context.Background(), k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete err = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(context.Background(), k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete err = %v, want ErrNotFound", err)
	}
}

func TestValidateIDsCanonicalises(t *testing.T) {
	ids, err := ValidateIDs([]string{" ABCDEF0123456789ABCDEF0123456789 ", "abcdef0123456789abcdef0123456789", "", "ABCDEF0123456789ABCDEF0123456789"})
	if err != nil {
		t.Fatalf("ValidateIDs: %v", err)
	}
	if fmt.Sprint(ids) != "[abcdef0123456789abcdef0123456789]" {
		t.Errorf("ids = %v, want lowercased dedupe of one value (uppercase duplicate folds in)", ids)
	}
	if _, err := ValidateIDs([]string{"nothex"}); err == nil {
		t.Errorf("invalid id accepted")
	}
	// 超上限：MaxKeys+1 个互不相同的合法 id
	var tooMany []string
	for i := 0; i < MaxKeys+1; i++ {
		tooMany = append(tooMany, fmt.Sprintf("%032x", i))
	}
	if _, err := ValidateIDs(tooMany); err == nil {
		t.Errorf("list over MaxKeys accepted")
	}
	if _, err := ValidateIDs(tooMany[:MaxKeys]); err != nil {
		t.Errorf("exactly MaxKeys rejected: %v", err)
	}
}

func TestMarshalIDsNilOnEmpty(t *testing.T) {
	if b := MarshalIDs(nil); b != nil {
		t.Errorf("MarshalIDs(nil) = %v, want nil (NULL column)", b)
	}
	if b := MarshalIDs([]string{}); b != nil {
		t.Errorf("MarshalIDs(empty) = %v, want nil (NULL column)", b)
	}
	b := MarshalIDs([]string{"a", "b"})
	if string(b) != `["a","b"]` {
		t.Errorf("MarshalIDs = %s", b)
	}
}

// makeTestKeyLine builds a structurally valid public key line for arbitrary
// types without ssh-keygen: manual SSH wire-format blob (len-prefixed type
// string + filler body), then the same parse path computes the expected
// fingerprint. Structural validity is all ParsePublicKey promises.
type testKeyLine struct {
	line        string
	fingerprint string
}

func makeWireString(s string) []byte {
	out := make([]byte, 4+len(s))
	binary.BigEndian.PutUint32(out[:4], uint32(len(s)))
	copy(out[4:], s)
	return out
}

func makeTestKeyLine(t *testing.T, keyType string) testKeyLine {
	t.Helper()
	blob := makeWireString(keyType)
	blob = append(blob, makeWireString("filler-body")...)
	line := keyType + " " + base64.StdEncoding.EncodeToString(blob) + " test"
	parsed, err := ParsePublicKey(line)
	if err != nil {
		t.Fatalf("makeTestKeyLine(%s): %v", keyType, err)
	}
	return testKeyLine{line: line, fingerprint: parsed.Fingerprint}
}
