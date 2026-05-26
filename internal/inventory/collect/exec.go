package collect

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// runCmd runs name with args under a per-call timeout. Returns combined output.
func runCmd(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return out, fmt.Errorf("%s %v: timeout after %s", name, args, timeout)
		}
		return out, fmt.Errorf("%s %v: %w (output=%q)", name, args, err, truncate(out, 200))
	}
	return out, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
