package installer

import (
	"context"
	"strings"
	"testing"
)

func TestNormalizeESPLayout_NoOpWhenTargetHasLoader(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	// Target dir espMount/EFI/openEuler/grubx64.efi already exists.
	// (On real FAT32, EFI and efi collapse to the same dir; the mock is
	// case-sensitive so we use the exact spelling normalizeESPLayout checks.)
	fs.files["/mnt/esp/EFI/openEuler/grubx64.efi"] = []byte("fake efi")
	// Source also exists but should be ignored.
	fs.files["/mnt/esp/efi/EFI/openEuler/grubx64.efi"] = []byte("nested efi")
	deps := Deps{Exec: exec, FS: fs, Reporter: &mockReporter{}}

	normalizeESPLayout(context.Background(), deps, "/mnt/esp", "openEuler")

	for _, c := range exec.Calls() {
		if c.Name == "cp" {
			t.Fatalf("cp should not be called when target already has loader; call=%v", c)
		}
	}
}

func TestNormalizeESPLayout_CopiesFromNested(t *testing.T) {
	exec := newMockExec()
	exec.On["cp"] = mockExecResult{}
	fs := newMockFS()
	// Target level empty; source has grubx64.efi.
	fs.files["/mnt/esp/efi/EFI/openEuler/grubx64.efi"] = []byte("fake efi")
	rep := &mockReporter{}
	deps := Deps{Exec: exec, FS: fs, Reporter: rep}

	normalizeESPLayout(context.Background(), deps, "/mnt/esp", "openEuler")

	var sawCp bool
	for _, c := range exec.Calls() {
		if c.Name == "cp" && len(c.Args) >= 3 {
			full := strings.Join(c.Args, " ")
			if strings.Contains(full, "/mnt/esp/efi/EFI/openEuler/.") &&
				strings.Contains(full, "/mnt/esp/EFI/openEuler/") {
				sawCp = true
			}
		}
	}
	if !sawCp {
		t.Fatalf("cp from nested efi/EFI/openEuler/ to EFI/openEuler/ not called; calls=%v", exec.Calls())
	}
	var sawLog bool
	for _, l := range rep.logs {
		if strings.Contains(l, "normalized ESP layout") {
			sawLog = true
		}
	}
	if !sawLog {
		t.Fatalf("expected normalize log; logs=%v", rep.logs)
	}
}

func TestNormalizeESPLayout_NoOpWhenNoSource(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	deps := Deps{Exec: exec, FS: fs, Reporter: &mockReporter{}}

	normalizeESPLayout(context.Background(), deps, "/mnt/esp", "openEuler")

	for _, c := range exec.Calls() {
		if c.Name == "cp" {
			t.Fatalf("cp should not be called when no source loader exists; call=%v", c)
		}
	}
}

func TestNormalizeESPLayout_EmptyArgsNoOp(t *testing.T) {
	exec := newMockExec()
	deps := Deps{Exec: exec, FS: newMockFS(), Reporter: &mockReporter{}}

	normalizeESPLayout(context.Background(), deps, "", "openEuler")
	normalizeESPLayout(context.Background(), deps, "/mnt/esp", "")

	for _, c := range exec.Calls() {
		if c.Name == "cp" {
			t.Fatalf("cp should not be called with empty args; call=%v", c)
		}
	}
}

func TestNormalizeESPLayout_CopiesBOOTFallback(t *testing.T) {
	exec := newMockExec()
	exec.On["cp"] = mockExecResult{}
	fs := newMockFS()
	// Source has loader for bootID.
	fs.files["/mnt/esp/efi/EFI/openEuler/grubx64.efi"] = []byte("efi")
	// Source also has BOOT fallback at nested level.
	fs.files["/mnt/esp/efi/EFI/BOOT/BOOTX64.EFI"] = []byte("boot efi")
	deps := Deps{Exec: exec, FS: fs, Reporter: &mockReporter{}}

	normalizeESPLayout(context.Background(), deps, "/mnt/esp", "openEuler")

	// Should have called cp for BOOT fallback too.
	bootCopies := 0
	for _, c := range exec.Calls() {
		if c.Name == "cp" {
			full := strings.Join(c.Args, " ")
			if strings.Contains(full, "BOOT") {
				bootCopies++
			}
		}
	}
	if bootCopies == 0 {
		t.Fatalf("expected cp of BOOT fallback dir; calls=%v", exec.Calls())
	}
}

func TestNormalizeESPLayout_CpFailureLogsWarn(t *testing.T) {
	exec := newMockExec()
	exec.On["cp"] = mockExecResult{Err: errString("cp: permission denied")}
	fs := newMockFS()
	fs.files["/mnt/esp/efi/EFI/openEuler/grubx64.efi"] = []byte("efi")
	rep := &mockReporter{}
	deps := Deps{Exec: exec, FS: fs, Reporter: rep}

	// Must not panic.
	normalizeESPLayout(context.Background(), deps, "/mnt/esp", "openEuler")

	// cp was attempted (and failed); no normalize log should appear.
	for _, l := range rep.logs {
		if strings.Contains(l, "normalized ESP layout") {
			t.Fatalf("should not log success when cp failed; logs=%v", rep.logs)
		}
	}
}
