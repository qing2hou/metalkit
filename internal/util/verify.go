package util

import (
	"context"
	"crypto/subtle"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// VerifyCryptSHA512 checks a plaintext password against a $6$ sha512crypt
// hash. The salt is extracted from the hash itself, the candidate is
// re-hashed with the same primitive (mkpasswd) and compared in constant
// time. Reuses the shell-out strategy of CryptSHA512 for the same reason:
// Go stdlib has no sha512crypt and adding x/crypto is a transitive dep the
// project has deliberately avoided so far.
//
// Login is a cold path (a handful of calls per day), so one fork per
// attempt is fine.
func VerifyCryptSHA512(ctx context.Context, password, hash string) (bool, error) {
	if !strings.HasPrefix(hash, "$6$") {
		return false, fmt.Errorf("verify: unsupported hash prefix %q", hash[:min(3, len(hash))])
	}
	// $6$<salt>$<digest> — salt runs to the last "$".
	last := strings.LastIndex(hash, "$")
	if last < 3 {
		return false, fmt.Errorf("verify: malformed hash")
	}
	salt := hash[3:last]

	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(callCtx, "mkpasswd", "-m", "sha-512", "-S", salt, "--stdin")
	cmd.Stdin = strings.NewReader(password)
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("mkpasswd: %w", err)
	}
	// Constant-time compare: the strings happen to be equal-length here
	// (same salt ⇒ same format), and the fork dominates timing anyway —
	// this is defense in depth, not the main mitigation.
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(out))), []byte(hash)) == 1, nil
}
