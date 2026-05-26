package util

import (
	"context"
	"crypto/sha512"
	"os/exec"
	"strings"
	"testing"
)

func mkpasswdAvail() bool {
	_, err := exec.LookPath("mkpasswd")
	return err == nil
}

func TestCryptSHA512_RoundTrip(t *testing.T) {
	if !mkpasswdAvail() {
		t.Skip("mkpasswd not on PATH")
	}
	h, err := CryptSHA512(context.Background(), "test-password-123")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.HasPrefix(h, "$6$") {
		t.Fatalf("hash prefix: %q", h)
	}
	// sanity: rerun with same password should produce different hash (different salt)
	h2, _ := CryptSHA512(context.Background(), "test-password-123")
	if h == h2 {
		t.Errorf("salt did not differ across calls (likely degenerate randomness): %q", h)
	}
	// not strictly required, but make sure hash length looks like sha512crypt
	if len(h) < 80 {
		t.Errorf("hash too short: %d", len(h))
	}
	_ = sha512.Sum512 // keep import to satisfy lint if added
}

func TestCryptSHA512_RejectShort(t *testing.T) {
	_, err := CryptSHA512(context.Background(), "short")
	if err == nil {
		t.Fatal("expected error for short password")
	}
}

func TestCryptSHA512_RejectLong(t *testing.T) {
	_, err := CryptSHA512(context.Background(), strings.Repeat("x", 200))
	if err == nil {
		t.Fatal("expected error for long password")
	}
}
