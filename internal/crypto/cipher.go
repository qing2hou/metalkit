// Package crypto provides field-level AES-256-GCM encryption for sensitive
// columns (BMC passwords today; expected to grow as more secrets appear).
//
// The cipher uses a single 32-byte master key persisted at a filesystem path
// (default /var/lib/metalkit/master.key, mode 0600). If the file exists at
// controller startup it is loaded; otherwise a fresh key is generated and
// written. There is no key rotation in M2 — the file is forever; back it up
// (it's 32 bytes, fits in a sticky note) or you lose every encrypted column.
//
// Ciphertext layout (per value):
//
//	byte 0       version (currently 0x01)
//	bytes 1..12  12-byte random nonce
//	bytes 13..   AES-GCM ciphertext || 16-byte tag
//
// The version byte exists so a future rotation scheme can be added without
// migrating existing rows in one go. Decrypt rejects unknown versions hard.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
)

// KeySize is 32 bytes (AES-256).
const KeySize = 32

// CurrentVersion is the version byte stamped on new ciphertexts.
const CurrentVersion byte = 0x01

// nonceSize is the standard 96-bit GCM nonce.
const nonceSize = 12

// LoadOrCreateMaster opens the key file at path, or creates a fresh 32-byte
// key if it doesn't exist. The file is always 0600. Returns the raw bytes;
// callers wrap them in NewCipher.
//
// Concurrency: this is meant to run once at controller boot, before any
// goroutine touches the cipher. It does NOT lock the file.
func LoadOrCreateMaster(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("crypto: master key path is empty")
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != KeySize {
			return nil, fmt.Errorf("crypto: %s has %d bytes, want %d", path, len(data), KeySize)
		}
		if err := assertPerm(path); err != nil {
			return nil, err
		}
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("crypto: read %s: %w", path, err)
	}

	// Generate + write.
	buf := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return nil, fmt.Errorf("crypto: generate master key: %w", err)
	}
	// O_CREATE|O_EXCL so two simultaneous controllers can't both create.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("crypto: create %s: %w", path, err)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("crypto: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("crypto: close %s: %w", path, err)
	}
	return buf, nil
}

// assertPerm refuses to load a key file with group/world bits set. We want
// loud failure rather than silently chugging along with a leaked key.
func assertPerm(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("crypto: stat %s: %w", path, err)
	}
	if mode := st.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("crypto: %s has perms %#o, want 0600 (group/world readable)", path, mode)
	}
	return nil
}

// Cipher encrypts and decrypts values using AES-GCM with the loaded master key.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher constructs a Cipher from a 32-byte key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: key is %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt produces version || nonce || ciphertext||tag.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: nonce: %w", err)
	}
	ct := c.aead.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, 1+nonceSize+len(ct))
	out = append(out, CurrentVersion)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// Decrypt reverses Encrypt. Returns an error for unknown version, truncated
// input, or tag mismatch (tampered ciphertext / wrong key).
func (c *Cipher) Decrypt(blob []byte) ([]byte, error) {
	if len(blob) < 1+nonceSize+c.aead.Overhead() {
		return nil, fmt.Errorf("crypto: ciphertext too short (%d bytes)", len(blob))
	}
	if blob[0] != CurrentVersion {
		return nil, fmt.Errorf("crypto: unsupported ciphertext version %#x", blob[0])
	}
	nonce := blob[1 : 1+nonceSize]
	ct := blob[1+nonceSize:]
	pt, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return pt, nil
}
