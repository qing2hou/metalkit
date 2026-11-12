package installer

import (
	"strings"
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

func TestValidateRequestedBootMode(t *testing.T) {
	cases := []struct {
		requested, detected string
		wantErr             bool
	}{
		{"", "uefi", false},
		{"", "bios", false},
		{"auto", "uefi", false},
		{"auto", "bios", false},
		{"uefi", "uefi", false},
		{"bios", "bios", false},
		{"uefi", "bios", true},
		{"bios", "uefi", true},
		{"legacy", "bios", true},
	}
	for _, tc := range cases {
		err := ValidateRequestedBootMode(tc.requested, tc.detected)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ValidateRequestedBootMode(%q, %q): expected error", tc.requested, tc.detected)
			}
		} else if err != nil {
			t.Errorf("ValidateRequestedBootMode(%q, %q): unexpected error %v", tc.requested, tc.detected, err)
		}
	}

	err := ValidateRequestedBootMode("bios", "uefi")
	if err == nil || !strings.Contains(err.Error(), "BMC/BIOS") {
		t.Fatalf("mismatch error is not actionable: %v", err)
	}
}
