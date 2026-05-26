// Package util holds small standalone utilities exposed via the admin HTTP
// API. Currently: SHA-512 crypt for hashing root passwords from the UI.
package util

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CryptSHA512 generates a fresh 16-char salt and shells out to mkpasswd
// (from the `whois` package) to produce a $6$… sha512crypt hash. The
// controller process owns the salt and password lifetime (in-memory only,
// never logged).
//
// Why shell out: Go stdlib has no SHA-512 crypt; pulling a crypt library
// would add a transitive dep. mkpasswd is already on every controller host
// (it's in the live image tool set too).
func CryptSHA512(ctx context.Context, password string) (string, error) {
	if len(password) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}
	if len(password) > 128 {
		return "", errors.New("password longer than 128 characters")
	}
	salt, err := randomSalt(16)
	if err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(callCtx, "mkpasswd", "-m", "sha-512", "-S", salt, "--stdin")
	cmd.Stdin = strings.NewReader(password)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("mkpasswd: %w", err)
	}
	hash := strings.TrimSpace(string(out))
	if !strings.HasPrefix(hash, "$6$") {
		return "", fmt.Errorf("unexpected mkpasswd output: %q", hash)
	}
	return hash, nil
}

func randomSalt(n int) (string, error) {
	// mkpasswd salt alphabet is [./A-Za-z0-9] (64 chars). Generate raw
	// bytes then map; cheaper than rejection sampling.
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding.EncodeToString(buf)
	// base64 alphabet "+/" → mkpasswd wants ".". Replace defensively.
	enc = strings.ReplaceAll(enc, "+", ".")
	enc = strings.ReplaceAll(enc, "/", ".")
	if len(enc) < n {
		return enc, nil
	}
	return enc[:n], nil
}
