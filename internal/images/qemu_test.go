package images

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestQemuImgAvailability(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := NewQemuImg(logger)
	if _, err := exec.LookPath("qemu-img"); err == nil {
		if !q.Available() {
			t.Errorf("qemu-img on PATH but Available()=false")
		}
	} else {
		if q.Available() {
			t.Errorf("qemu-img not on PATH but Available()=true")
		}
		_, _, _, err := q.Extract("/nonexistent")
		if !errors.Is(err, ErrQemuUnavailable) {
			t.Errorf("Extract: err=%v want ErrQemuUnavailable", err)
		}
	}
}

func TestQemuImgExtractRaw(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := NewQemuImg(logger)

	// Create a tiny raw file (1 MiB of zeros). qemu-img auto-detects format
	// and reports virtual_size = file size for raw.
	path := filepath.Join(t.TempDir(), "x.raw")
	if err := exec.Command("qemu-img", "create", "-f", "raw", path, "1M").Run(); err != nil {
		t.Fatalf("qemu-img create: %v", err)
	}

	format, virtSize, meta, err := q.Extract(path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if format != "raw" {
		t.Errorf("format: got %q want %q", format, "raw")
	}
	if virtSize != 1024*1024 {
		t.Errorf("virtual-size: got %d want %d", virtSize, 1024*1024)
	}
	if !strings.Contains(meta, `"format"`) {
		t.Errorf("metadata json missing format key: %s", meta)
	}
}

func TestQemuImgExtractQcow2(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := NewQemuImg(logger)

	path := filepath.Join(t.TempDir(), "x.qcow2")
	if err := exec.Command("qemu-img", "create", "-f", "qcow2", path, "4M").Run(); err != nil {
		t.Fatalf("qemu-img create: %v", err)
	}

	format, virtSize, _, err := q.ExtractWithCtx(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if format != "qcow2" {
		t.Errorf("format: got %q want qcow2", format)
	}
	if virtSize != 4*1024*1024 {
		t.Errorf("virtual-size: got %d want %d", virtSize, 4*1024*1024)
	}
}

func TestQemuImgExtractNonExistent(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := NewQemuImg(logger)
	if _, _, _, err := q.Extract("/no/such/file"); err == nil {
		t.Fatal("Extract on missing file: want error")
	}
}

func TestQemuImgExtractGarbage(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := NewQemuImg(logger)

	path := filepath.Join(t.TempDir(), "garbage")
	// Write a few bytes that aren't a recognised image. qemu-img falls back to
	// treating it as raw, so this is more a sanity check than a hard error case.
	if err := exec.Command("dd", "if=/dev/zero", "of="+path, "bs=512", "count=1").Run(); err != nil {
		t.Fatalf("dd: %v", err)
	}
	format, virtSize, _, err := q.Extract(path)
	if err != nil {
		t.Fatalf("Extract on raw bytes: %v", err)
	}
	if format == "" || virtSize == 0 {
		t.Errorf("expected raw fallback, got format=%q size=%d", format, virtSize)
	}
}
