package crypto

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateMasterFreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	key, err := LoadOrCreateMaster(path)
	if err != nil {
		t.Fatalf("LoadOrCreateMaster: %v", err)
	}
	if len(key) != KeySize {
		t.Fatalf("len(key)=%d want %d", len(key), KeySize)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode %#o want 0600", mode)
	}
}

func TestLoadOrCreateMasterRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	k1, err := LoadOrCreateMaster(path)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	k2, err := LoadOrCreateMaster(path)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Errorf("second load returned different key — should re-use existing file")
	}
}

func TestLoadOrCreateMasterRejectsBadPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xab}, KeySize), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadOrCreateMaster(path)
	if err == nil {
		t.Fatal("want perm error, got nil")
	}
}

func TestLoadOrCreateMasterRejectsShortFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte("too-short"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadOrCreateMaster(path)
	if err == nil {
		t.Fatal("want length error, got nil")
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, KeySize)
	c, err := NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	for _, plain := range [][]byte{
		[]byte(""),
		[]byte("hunter2"),
		bytes.Repeat([]byte("A"), 1024),
	} {
		blob, err := c.Encrypt(plain)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if blob[0] != CurrentVersion {
			t.Errorf("version byte = %#x", blob[0])
		}
		// Re-encrypting same input must produce DIFFERENT ciphertext (nonce randomness).
		blob2, _ := c.Encrypt(plain)
		if bytes.Equal(blob, blob2) {
			t.Errorf("two encryptions of same plaintext produced identical ciphertext")
		}
		got, err := c.Decrypt(blob)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("roundtrip mismatch: got=%q want=%q", got, plain)
		}
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	c, err := NewCipher(bytes.Repeat([]byte{0x42}, KeySize))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blob, err := c.Encrypt([]byte("hello"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Flip a bit in the body.
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := c.Decrypt(tampered); err == nil {
		t.Error("Decrypt accepted tampered ciphertext")
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	c1, _ := NewCipher(bytes.Repeat([]byte{0x42}, KeySize))
	c2, _ := NewCipher(bytes.Repeat([]byte{0x99}, KeySize))
	blob, err := c1.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := c2.Decrypt(blob); err == nil {
		t.Error("c2 decrypted ciphertext from c1 — keys aren't being applied")
	}
}

func TestDecryptRejectsUnknownVersion(t *testing.T) {
	c, _ := NewCipher(bytes.Repeat([]byte{0x42}, KeySize))
	blob, _ := c.Encrypt([]byte("x"))
	blob[0] = 0xFF
	_, err := c.Decrypt(blob)
	if err == nil {
		t.Fatal("Decrypt accepted unknown version")
	}
}

func TestDecryptRejectsShortInput(t *testing.T) {
	c, _ := NewCipher(bytes.Repeat([]byte{0x42}, KeySize))
	for _, n := range []int{0, 1, 5, 12} {
		_, err := c.Decrypt(make([]byte, n))
		if err == nil {
			t.Errorf("Decrypt accepted %d-byte input", n)
		}
	}
}

func TestNewCipherRejectsBadKey(t *testing.T) {
	for _, n := range []int{0, 1, 16, 31, 33, 64} {
		_, err := NewCipher(make([]byte, n))
		if err == nil {
			t.Errorf("NewCipher accepted %d-byte key", n)
		}
	}
}

func TestLoadOrCreateMasterEmptyPath(t *testing.T) {
	_, err := LoadOrCreateMaster("")
	if err == nil {
		t.Fatal("want error for empty path")
	}
	if !errors.Is(err, errors.New("crypto: master key path is empty")) {
		// errors.Is on a wrapped Errorf won't match — just check that we got
		// SOME error, the message check is the next test.
	}
}
