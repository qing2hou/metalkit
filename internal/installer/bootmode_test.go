package installer

import (
	"testing"
)

func TestDetectBootMode(t *testing.T) {
	t.Run("uefi when efivars dir exists", func(t *testing.T) {
		fs := newMockFS()
		_ = fs.MkdirAll("/sys/firmware/efi/efivars", 0o755)
		mode, err := DetectBootMode(fs)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if mode != "uefi" {
			t.Fatalf("want uefi, got %q", mode)
		}
	})

	t.Run("bios when efivars missing", func(t *testing.T) {
		fs := newMockFS()
		mode, err := DetectBootMode(fs)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if mode != "bios" {
			t.Fatalf("want bios, got %q", mode)
		}
	})

	t.Run("bios when efivars is a file not a dir", func(t *testing.T) {
		fs := newMockFS()
		fs.notADir["/sys/firmware/efi/efivars"] = true
		mode, err := DetectBootMode(fs)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if mode != "bios" {
			t.Fatalf("want bios when path is not a dir, got %q", mode)
		}
	})

	t.Run("nil fs returns error", func(t *testing.T) {
		if _, err := DetectBootMode(nil); err == nil {
			t.Fatal("expected error for nil fs")
		}
	})
}

func TestRequireUEFI(t *testing.T) {
	fs := newMockFS()
	if err := RequireUEFI(fs); err == nil {
		t.Fatal("expected BIOS to fail RequireUEFI")
	}
	_ = fs.MkdirAll("/sys/firmware/efi/efivars", 0o755)
	if err := RequireUEFI(fs); err != nil {
		t.Fatalf("UEFI environment should pass: %v", err)
	}
}
