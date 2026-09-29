package util

import (
	"context"
	"os/exec"
	"testing"
)

// The whole file needs mkpasswd on PATH; skip silently when absent (e.g.
// minimal CI containers) rather than failing the suite.
func requireMkpasswd(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mkpasswd"); err != nil {
		t.Skip("mkpasswd not in PATH")
	}
}

func TestVerifyCryptSHA512_RoundTrip(t *testing.T) {
	requireMkpasswd(t)
	ctx := context.Background()

	hash, err := CryptSHA512(ctx, "s3cret-password")
	if err != nil {
		t.Fatalf("CryptSHA512: %v", err)
	}

	ok, err := VerifyCryptSHA512(ctx, "s3cret-password", hash)
	if err != nil || !ok {
		t.Errorf("correct password: ok=%v err=%v", ok, err)
	}

	ok, err = VerifyCryptSHA512(ctx, "wrong-password", hash)
	if err != nil {
		t.Fatalf("Verify wrong: %v", err)
	}
	if ok {
		t.Errorf("wrong password verified — verifier broken")
	}
}

func TestVerifyCryptSHA512_MalformedHash(t *testing.T) {
	requireMkpasswd(t)
	if _, err := VerifyCryptSHA512(context.Background(), "x", "not-a-hash"); err == nil {
		t.Errorf("malformed hash: expected error")
	}
	if _, err := VerifyCryptSHA512(context.Background(), "x", "$6$"); err == nil {
		t.Errorf("truncated hash: expected error")
	}
}
