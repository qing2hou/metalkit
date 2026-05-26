package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// QemuImg wraps the `qemu-img` CLI. The binary is detected at construction
// time: if it's missing, calls to Extract return ErrQemuUnavailable and the
// caller (uploads.FinalizeUpload) records the image with fallback metadata
// rather than failing the upload. This keeps M2.2 useful on hosts that
// don't have qemu-utils yet, with a clearly logged degradation.
type QemuImg struct {
	binary  string // resolved absolute path, empty when not installed
	logger  *slog.Logger
	timeout time.Duration
}

// ErrQemuUnavailable is returned by Extract when the qemu-img binary is not
// installed on PATH. It is not an error in the catalog sense — uploads.go
// degrades gracefully on this signal.
var ErrQemuUnavailable = errors.New("images: qemu-img not installed")

// NewQemuImg locates qemu-img on PATH. If not found, the returned wrapper
// is still usable (Available() == false) and Extract returns
// ErrQemuUnavailable, so callers can decide whether to fail or degrade.
func NewQemuImg(logger *slog.Logger) *QemuImg {
	q := &QemuImg{logger: logger, timeout: 30 * time.Second}
	if path, err := exec.LookPath("qemu-img"); err == nil {
		q.binary = path
	}
	return q
}

// Available reports whether qemu-img was found on PATH.
func (q *QemuImg) Available() bool { return q.binary != "" }

// qemuImgInfo is the subset of `qemu-img info --output=json` we care about.
type qemuImgInfo struct {
	VirtualSize int64  `json:"virtual-size"`
	Filename    string `json:"filename"`
	Format      string `json:"format"`
	ActualSize  int64  `json:"actual-size"`
}

// Extract runs `qemu-img info --output=json` against path and returns
// (format, virtual_size, raw_json). The raw json is preserved so the UI can
// surface arbitrary metadata fields without the controller having to know
// them all up front.
//
// Errors from qemu-img surface verbatim — the caller logs and degrades.
func (q *QemuImg) Extract(path string) (string, int64, string, error) {
	return q.ExtractWithCtx(context.Background(), path)
}

func (q *QemuImg) ExtractWithCtx(ctx context.Context, path string) (string, int64, string, error) {
	if !q.Available() {
		return "", 0, "", ErrQemuUnavailable
	}
	if _, err := os.Stat(path); err != nil {
		return "", 0, "", fmt.Errorf("stat image: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()

	// --force-share lets qemu-img inspect an image that's locked by another
	// process. We're inspecting an immutable post-upload file so this only
	// matters in pathological cases, but cheap to add.
	cmd := exec.CommandContext(ctx, q.binary, "info", "--output=json", "--force-share", path)
	out, err := cmd.Output()
	if err != nil {
		// stderr is in *exec.ExitError.Stderr when CombinedOutput wasn't used.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", 0, "", fmt.Errorf("qemu-img info: %w: %s", err, string(ee.Stderr))
		}
		return "", 0, "", fmt.Errorf("qemu-img info: %w", err)
	}

	var parsed qemuImgInfo
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", 0, "", fmt.Errorf("parse qemu-img info: %w", err)
	}
	return parsed.Format, parsed.VirtualSize, string(out), nil
}
