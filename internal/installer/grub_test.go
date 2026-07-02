package installer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"metalkit/internal/jobs"
	"metalkit/internal/profiles"
)

func TestInstallGRUB_UEFI_HappyPath(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "ubuntu"}}, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// No GRUB cmdline overlay.
	if _, ok := fs.files["/mnt/root/etc/default/grub.d/99-metalkit.cfg"]; ok {
		t.Fatal("99-metalkit.cfg overlay must not be written")
	}

	// Host grub-install for UEFI, then chroot update-grub, then efibootmgr.
	var sawInstall, sawUpdate, sawEFI bool
	for _, c := range exec.Calls() {
		switch c.Name {
		case "grub-install":
			sawInstall = true
			full := strings.Join(c.Args, " ")
			if !strings.Contains(full, "--target=x86_64-efi") {
				t.Fatalf("grub-install missing UEFI target: %v", c.Args)
			}
			if !strings.Contains(full, "--efi-directory=/mnt/root/boot/efi") {
				t.Fatalf("grub-install missing efi-directory: %v", c.Args)
			}
			if !strings.Contains(full, "--boot-directory=/mnt/root/boot") {
				t.Fatalf("grub-install missing boot-directory: %v", c.Args)
			}
			if !strings.Contains(full, "--bootloader-id=metalkit") {
				t.Fatalf("grub-install missing bootloader-id: %v", c.Args)
			}
		case "chroot":
			if len(c.Args) >= 2 && c.Args[0] == "/mnt/root" && c.Args[1] == "update-grub" {
				sawUpdate = true
			}
		case "efibootmgr":
			sawEFI = true
		}
	}
	if !sawInstall {
		t.Fatal("grub-install not invoked")
	}
	if !sawUpdate {
		t.Fatal("update-grub not invoked")
	}
	if !sawEFI {
		t.Fatal("efibootmgr not invoked")
	}
}

func TestInstallGRUB_BIOS_HappyPath(t *testing.T) {
	exec := newMockExec()
	deps := Deps{Exec: exec, FS: newMockFS()}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "ubuntu"}}, "/mnt/root", "/dev/sda", ""); err != nil {
		t.Fatalf("BIOS install should succeed: %v", err)
	}

	var sawBIOS, sawUpdate bool
	for _, c := range exec.Calls() {
		switch c.Name {
		case "grub-install":
			sawBIOS = true
			full := strings.Join(c.Args, " ")
			if !strings.Contains(full, "--target=i386-pc") {
				t.Fatalf("BIOS grub-install missing --target=i386-pc: %v", c.Args)
			}
			if !strings.Contains(full, "--boot-directory=/mnt/root/boot") {
				t.Fatalf("BIOS grub-install missing boot-directory: %v", c.Args)
			}
			if c.Args[len(c.Args)-1] != "/dev/sda" {
				t.Fatalf("BIOS grub-install missing disk target /dev/sda: %v", c.Args)
			}
		case "chroot":
			if len(c.Args) >= 2 && c.Args[0] == "/mnt/root" && c.Args[1] == "update-grub" {
				sawUpdate = true
			}
		case "efibootmgr":
			t.Fatal("efibootmgr must not be called on BIOS path")
		}
	}
	if !sawBIOS {
		t.Fatal("BIOS grub-install not invoked")
	}
	if !sawUpdate {
		t.Fatal("update-grub not invoked")
	}
}

func TestInstallGRUB_GrubInstallFailureSurfaces(t *testing.T) {
	exec := newMockExec()
	exec.On["grub-install"] = mockExecResult{Err: errString("device not found")}
	deps := Deps{Exec: exec, FS: newMockFS()}
	err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "ubuntu"}}, "/mnt/root", "/dev/sda", "/boot/efi")
	if err == nil {
		t.Fatal("grub-install failure must surface")
	}
}

func TestInstallGRUB_UpdateGrubFallsBackToHost(t *testing.T) {
	exec := newMockExec()
	// All chroot methods fail, host grub-mkconfig succeeds.
	exec.OnFull["chroot /mnt/root update-grub"] = mockExecResult{Err: errString("not found")}
	exec.OnFull["chroot /mnt/root grub2-mkconfig -o /boot/grub2/grub.cfg"] = mockExecResult{Err: errString("not found")}
	exec.OnFull["chroot /mnt/root grub-mkconfig -o /boot/grub/grub.cfg"] = mockExecResult{Err: errString("not found")}
	deps := Deps{Exec: exec, FS: newMockFS()}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "ubuntu"}}, "/mnt/root", "/dev/sda", ""); err != nil {
		t.Fatalf("BIOS install with host grub-mkconfig fallback should succeed: %v", err)
	}

	// Verify host grub-mkconfig was called.
	var sawHostMKConfig bool
	for _, c := range exec.Calls() {
		if c.Name == "grub-mkconfig" {
			sawHostMKConfig = true
			full := strings.Join(c.Args, " ")
			if !strings.Contains(full, "/mnt/root/boot/grub/grub.cfg") {
				t.Fatalf("host grub-mkconfig should target /mnt/root/boot/grub/grub.cfg: %v", c.Args)
			}
		}
	}
	if !sawHostMKConfig {
		t.Fatal("host grub-mkconfig fallback was not invoked")
	}
}

func TestInstallGRUB_AllUpdateGrubMethodsFail(t *testing.T) {
	exec := newMockExec()
	// All chroot methods fail, host grub-mkconfig also fails — install should still succeed.
	exec.OnFull["chroot /mnt/root update-grub"] = mockExecResult{Err: errString("not found")}
	exec.OnFull["chroot /mnt/root grub2-mkconfig -o /boot/grub2/grub.cfg"] = mockExecResult{Err: errString("not found")}
	exec.OnFull["chroot /mnt/root grub-mkconfig -o /boot/grub/grub.cfg"] = mockExecResult{Err: errString("not found")}
	exec.On["grub-mkconfig"] = mockExecResult{Err: errString("overlay")}
	deps := Deps{Exec: exec, FS: newMockFS(), Logger: testLogger(t)}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "ubuntu"}}, "/mnt/root", "/dev/sda", ""); err != nil {
		t.Fatalf("install should succeed even when all update-grub methods fail: %v", err)
	}
}

func TestInstallGRUB_RHELGrub2Symlink(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	// Pretend the CentOS image has a /boot/grub2/grub.cfg already.
	fs.files["/mnt/root/boot/grub2/grub.cfg"] = []byte("set root=...")
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "ubuntu"}}, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// A symlink must have been created from /boot/grub/grub.cfg → ../grub2/grub.cfg
	data, err := fs.ReadFile("/mnt/root/boot/grub/grub.cfg")
	if err != nil {
		t.Fatalf("grub.cfg symlink missing: %v", err)
	}
	if string(data) != "symlink:../grub2/grub.cfg" {
		t.Fatalf("unexpected symlink target: %s", string(data))
	}
}

// --- RHEL-family chroot-grub2 path ---------------------------------------

// When the target rootfs is modern RHEL family (Rocky/Alma/RHEL8+/Fedora)
// on UEFI, we do NOT run grub2-install — RHEL's grub2-install refuses on
// EFI platforms because it can't sign for Secure Boot. Instead we trust
// the image's pre-baked /EFI/<id>/shim*.efi and just register an NVRAM
// entry pointing at it, plus regenerate /boot/grub2/grub.cfg.
func TestInstallGRUB_RockyTarget_UEFI_RegistersShimViaEfibootmgr(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/usr/sbin/grub2-install"] = []byte("#!/bin/sh\nexit 0\n")
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="rocky"` + "\n" + `VERSION_ID="9.3"` + "\n")
	// Pre-baked EFI tree from the cloud image.
	fs.dirs["/mnt/root/boot/efi/EFI/rocky"] = true
	fs.files["/mnt/root/boot/efi/EFI/rocky/shimx64.efi"] = []byte("shim")
	fs.files["/mnt/root/boot/efi/EFI/rocky/grubx64.efi"] = []byte("grub")
	// /proc/mounts so we can resolve the ESP partition number.
	fs.files["/proc/mounts"] = []byte("/dev/sda2 /mnt/root/boot/efi vfat rw 0 0\n")
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "rhel"}}, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var sawHostGrubInstall, sawChrootGrub2Install, sawEfibootmgrCreate, sawMkconfig bool
	for _, c := range exec.Calls() {
		full := c.Name + " " + strings.Join(c.Args, " ")
		switch c.Name {
		case "grub-install":
			sawHostGrubInstall = true
		case "chroot":
			if strings.Contains(full, "grub2-install") {
				sawChrootGrub2Install = true
			}
			if strings.Contains(full, "grub2-mkconfig") && strings.Contains(full, "/boot/grub2/grub.cfg") {
				sawMkconfig = true
			}
		case "efibootmgr":
			if hasArg(c.Args, "--create") {
				sawEfibootmgrCreate = true
				if !hasFlagValue(c.Args, "--disk", "/dev/sda") {
					t.Fatalf("efibootmgr --disk should be /dev/sda, got %v", c.Args)
				}
				if !hasFlagValue(c.Args, "--part", "2") {
					t.Fatalf("efibootmgr --part should be 2 (from /dev/sda2 in /proc/mounts), got %v", c.Args)
				}
				if !hasFlagValue(c.Args, "--loader", `\EFI\rocky\shimx64.efi`) {
					t.Fatalf("efibootmgr --loader should point at shim, got %v", c.Args)
				}
				if !hasFlagValue(c.Args, "--label", "Rocky Linux") {
					t.Fatalf("efibootmgr --label should be 'Rocky Linux', got %v", c.Args)
				}
			}
		}
	}
	if sawHostGrubInstall {
		t.Fatal("host grub-install must NOT be invoked for Rocky UEFI")
	}
	if sawChrootGrub2Install {
		t.Fatal("chroot grub2-install must NOT be invoked for Rocky UEFI (refuses on EFI)")
	}
	if !sawEfibootmgrCreate {
		t.Fatal("efibootmgr --create was not invoked")
	}
	if !sawMkconfig {
		t.Fatal("chroot grub2-mkconfig was not invoked")
	}
}

// Rocky UEFI without an ESP EFI dir for the detected id falls back to
// chroot grub2-install. If both fail, the error surfaces.
func TestInstallGRUB_RockyTarget_UEFI_MissingShimFallsBackToGrub2Install(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/usr/sbin/grub2-install"] = []byte("#!/bin/sh\nexit 0\n")
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="rocky"` + "\n")
	// No /boot/efi/EFI/rocky at all — forces fallback to grub2-install.
	fs.files["/proc/mounts"] = []byte("/dev/sda2 /mnt/root/boot/efi vfat rw 0 0\n")
	deps := Deps{Exec: exec, FS: fs}

	// With mock exec (unregistered commands succeed), grub2-install fallback
	// should succeed and the overall install should not error.
	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "rhel"}}, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
		t.Fatalf("expected grub2-install fallback to succeed, got: %v", err)
	}
}

// Falls back from shimx64.efi → shim.efi → grubx64.efi based on what's
// actually on the ESP.
func TestInstallGRUB_RockyTarget_UEFI_FallsBackToGrubWhenShimAbsent(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/usr/sbin/grub2-install"] = []byte("#!/bin/sh\nexit 0\n")
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="rocky"` + "\n")
	// Only grubx64.efi present — shim missing.
	fs.dirs["/mnt/root/boot/efi/EFI/rocky"] = true
	fs.files["/mnt/root/boot/efi/EFI/rocky/grubx64.efi"] = []byte("grub")
	fs.files["/proc/mounts"] = []byte("/dev/nvme0n1p1 /mnt/root/boot/efi vfat rw 0 0\n")
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "rhel"}}, "/mnt/root", "/dev/nvme0n1", "/boot/efi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var loaderArg, partArg string
	for _, c := range exec.Calls() {
		if c.Name != "efibootmgr" || !hasArg(c.Args, "--create") {
			continue
		}
		for i, a := range c.Args {
			if a == "--loader" && i+1 < len(c.Args) {
				loaderArg = c.Args[i+1]
			}
			if a == "--part" && i+1 < len(c.Args) {
				partArg = c.Args[i+1]
			}
		}
	}
	if loaderArg != `\EFI\rocky\grubx64.efi` {
		t.Fatalf("loader should fall back to grubx64.efi, got %q", loaderArg)
	}
	if partArg != "1" {
		t.Fatalf("nvme0n1p1 partition number should be 1, got %q", partArg)
	}
}

// On reinstall, stale NVRAM entries must be removed before creating a fresh one.
// This prevents boot failures when the partition PARTUUID changes after a new image write.
func TestInstallGRUB_RockyTarget_UEFI_RemovesStaleEntryAndRecreates(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/usr/sbin/grub2-install"] = []byte("#!/bin/sh\nexit 0\n")
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="rocky"` + "\n")
	fs.dirs["/mnt/root/boot/efi/EFI/rocky"] = true
	fs.files["/mnt/root/boot/efi/EFI/rocky/shimx64.efi"] = []byte("shim")
	fs.files["/proc/mounts"] = []byte("/dev/sda2 /mnt/root/boot/efi vfat rw 0 0\n")
	// efibootmgr (no args) → list call returns a stale matching entry.
	exec.On["efibootmgr"] = mockExecResult{Out: []byte(
		"BootCurrent: 0001\n" +
			"Boot0001* Rocky Linux\tHD(2,GPT,...,0x1)/File(\\EFI\\rocky\\shimx64.efi)\n",
	)}
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "rhel"}}, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// Must have deleted the stale entry.
	foundDelete := false
	for _, c := range exec.Calls() {
		if c.Name == "efibootmgr" && hasArg(c.Args, "-B") {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Fatal("efibootmgr -B should be called to remove stale entry before recreating")
	}

	// Must have created a fresh entry.
	foundCreate := false
	for _, c := range exec.Calls() {
		if c.Name == "efibootmgr" && hasArg(c.Args, "--create") {
			foundCreate = true
		}
	}
	if !foundCreate {
		t.Fatal("efibootmgr --create should be called to create fresh entry after removing stale one")
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func hasFlagValue(args []string, flag, want string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == want {
			return true
		}
	}
	return false
}

// CentOS 7 ships grub2-install but is intentionally kept on the host
// grub-install path because that's the known-working production path.
// Don't promote it to chroot grub2-install until we've validated it on
// real CentOS 7 hardware.
func TestInstallGRUB_CentOS7Target_StaysOnHostGrubInstall(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/usr/sbin/grub2-install"] = []byte("#!/bin/sh\nexit 0\n")
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="centos"` + "\n" + `VERSION_ID="7"` + "\n")
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "rhel7"}}, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var sawHostInstall, sawChrootGrub2 bool
	for _, c := range exec.Calls() {
		if c.Name == "grub-install" {
			sawHostInstall = true
		}
		if c.Name == "chroot" && strings.Contains(strings.Join(c.Args, " "), "grub2-install") {
			sawChrootGrub2 = true
		}
	}
	if !sawHostInstall {
		t.Fatal("CentOS 7 should still use host grub-install (known-working path)")
	}
	if sawChrootGrub2 {
		t.Fatal("CentOS 7 must NOT take the chroot grub2-install path yet")
	}
}

// BIOS Rocky target: chroot grub2-install --target=i386-pc <disk> with
// no efi-directory.
func TestInstallGRUB_RockyTarget_BIOS(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/usr/sbin/grub2-install"] = []byte("#!/bin/sh\nexit 0\n")
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="rocky"` + "\n")
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "rhel"}}, "/mnt/root", "/dev/sda", ""); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var sawBIOS bool
	for _, c := range exec.Calls() {
		if c.Name != "chroot" {
			continue
		}
		full := strings.Join(c.Args, " ")
		if !strings.Contains(full, "grub2-install") {
			continue
		}
		if !strings.Contains(full, "--target=i386-pc") {
			t.Fatalf("Rocky BIOS chroot grub2-install missing --target=i386-pc: %v", c.Args)
		}
		if c.Args[len(c.Args)-1] != "/dev/sda" {
			t.Fatalf("Rocky BIOS chroot grub2-install missing disk target: %v", c.Args)
		}
		sawBIOS = true
	}
	if !sawBIOS {
		t.Fatal("chroot grub2-install (BIOS) not invoked")
	}
}

// Ubuntu target (no grub2-install in target rootfs) still uses the host
// grub-install path — guard against regressions.
func TestInstallGRUB_UbuntuTarget_UsesHostGrubInstall(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID=ubuntu` + "\n" + `VERSION_ID="22.04"` + "\n")
	// Crucially: no /usr/sbin/grub2-install in target.
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "ubuntu"}}, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var sawHostInstall, sawChrootGrub2Install bool
	for _, c := range exec.Calls() {
		if c.Name == "grub-install" {
			sawHostInstall = true
		}
		if c.Name == "chroot" && strings.Contains(strings.Join(c.Args, " "), "grub2-install") {
			sawChrootGrub2Install = true
		}
	}
	if !sawHostInstall {
		t.Fatal("Ubuntu target should still use host grub-install")
	}
	if sawChrootGrub2Install {
		t.Fatal("Ubuntu target must NOT invoke chroot grub2-install")
	}
}

func TestInstallGRUB_GrubCfgAlreadyExists_NoOverwrite(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	// Both grub.cfg files exist — no symlink should be created.
	fs.files["/mnt/root/boot/grub2/grub.cfg"] = []byte("centos cfg")
	fs.files["/mnt/root/boot/grub/grub.cfg"] = []byte("existing cfg")
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "openeuler"}}, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// The existing grub.cfg must NOT be overwritten.
	data, err := fs.ReadFile("/mnt/root/boot/grub/grub.cfg")
	if err != nil {
		t.Fatalf("grub.cfg missing: %v", err)
	}
	if string(data) != "existing cfg" {
		t.Fatalf("existing grub.cfg was overwritten, got: %s", string(data))
	}
}

// --- openEuler tests --------------------------------------------------------

// openEuler ships ID="openEuler" (capital E). Verify that shouldChrootGrub2
// matches it case-insensitively and takes the chroot grub2-install path.
func TestInstallGRUB_OpenEuler_CapitalE_MatchesChrootPath(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/usr/sbin/grub2-install"] = []byte("#!/bin/sh\nexit 0\n")
	// Real openEuler 24.03 os-release: ID="openEuler" with capital E.
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="openEuler"` + "\n" + `VERSION_ID="24.03"` + "\n")
	fs.files["/proc/mounts"] = []byte("/dev/sda1 /mnt/root/boot/efi vfat rw 0 0\n")
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "openeuler"}}, "/mnt/root", "/dev/sda", "/mnt/root/boot/efi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var sawHostInstall, sawChrootGrub2 bool
	for _, c := range exec.Calls() {
		if c.Name == "grub-install" {
			sawHostInstall = true
		}
		if c.Name == "chroot" && strings.Contains(strings.Join(c.Args, " "), "grub2-install") {
			sawChrootGrub2 = true
		}
	}
	if sawHostInstall {
		t.Fatal("openEuler must NOT use host grub-install (should use chroot grub2-install)")
	}
	if !sawChrootGrub2 {
		t.Fatal("openEuler must use chroot grub2-install path")
	}
}

// Verify detectRHELBootloaderID returns "openEuler" (capital E) to match the
// actual ESP directory name on openEuler images.
func TestDetectRHELBootloaderID_OpenEulerCapitalE(t *testing.T) {
	fs := newMockFS()
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="openEuler"` + "\n")
	deps := Deps{FS: fs}

	got := detectRHELBootloaderID(deps, "/mnt/root")
	if got != "openEuler" {
		t.Fatalf("expected openEuler (capital E), got %q", got)
	}
}

// Also match lowercase ID="openeuler" for older/alternative builds.
func TestDetectRHELBootloaderID_OpenEulerLowercase(t *testing.T) {
	fs := newMockFS()
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="openeuler"` + "\n")
	deps := Deps{FS: fs}

	got := detectRHELBootloaderID(deps, "/mnt/root")
	if got != "openEuler" {
		t.Fatalf("expected openEuler (canonical EFI dir name), got %q", got)
	}
}

// openEuler UEFI with ESP at /boot (not /boot/efi). This is the real-world
// layout: openEuler fstab declares UUID=... /boot vfat. The chroot grub2-install
// must use --efi-directory=/boot, not /boot/efi.
func TestInstallGRUB_OpenEuler_UEFI_BootMountPoint(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/usr/sbin/grub2-install"] = []byte("#!/bin/sh\nexit 0\n")
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="openEuler"` + "\n")
	fs.files["/proc/mounts"] = []byte("/dev/sda1 /mnt/root/boot vfat rw 0 0\n")
	deps := Deps{Exec: exec, FS: fs}

	// espMount is /mnt/root/boot, not /mnt/root/boot/efi.
	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "openeuler"}}, "/mnt/root", "/dev/sda", "/mnt/root/boot"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// Verify chroot grub2-install uses --efi-directory=/boot (not /boot/efi).
	var sawChrootInstall bool
	for _, c := range exec.Calls() {
		if c.Name != "chroot" {
			continue
		}
		full := strings.Join(c.Args, " ")
		if !strings.Contains(full, "grub2-install") {
			continue
		}
		sawChrootInstall = true
		if !strings.Contains(full, "--efi-directory=/boot") {
			t.Fatalf("openEuler with ESP at /boot must use --efi-directory=/boot, got: %v", c.Args)
		}
		if strings.Contains(full, "--efi-directory=/boot/efi") {
			t.Fatal("must NOT use --efi-directory=/boot/efi when ESP is at /boot")
		}
		if !strings.Contains(full, "--no-nvram") {
			t.Fatal("chroot grub2-install must use --no-nvram (NVRAM registered separately)")
		}
		if !strings.Contains(full, "--bootloader-id=openEuler") {
			t.Fatalf("bootloader-id must be openEuler (capital E), got: %v", c.Args)
		}
	}
	if !sawChrootInstall {
		t.Fatal("chroot grub2-install not invoked")
	}
}

// openEuler UEFI fallback: no pre-baked EFI dir for openEuler on the ESP,
// so registerEFIBootEntryRHEL fails and we fall back to chroot grub2-install.
func TestInstallGRUB_OpenEuler_UEFI_NoPrebakedEFI_FallbackToGrub2Install(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/usr/sbin/grub2-install"] = []byte("#!/bin/sh\nexit 0\n")
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="openEuler"` + "\n")
	// No /boot/efi/EFI/openEuler and no /boot/EFI/openEuler — forces fallback.
	fs.files["/proc/mounts"] = []byte("/dev/sda1 /mnt/root/boot vfat rw 0 0\n")
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "openeuler"}}, "/mnt/root", "/dev/sda", "/mnt/root/boot"); err != nil {
		t.Fatalf("expected grub2-install fallback to succeed, got: %v", err)
	}

	// Verify the fallback chroot grub2-install was called.
	var sawFallback bool
	for _, c := range exec.Calls() {
		if c.Name == "chroot" {
			full := strings.Join(c.Args, " ")
			if strings.Contains(full, "grub2-install") && strings.Contains(full, "--efi-directory=/boot") {
				sawFallback = true
			}
		}
	}
	if !sawFallback {
		t.Fatal("fallback chroot grub2-install was not invoked")
	}
}

// openEuler BIOS: chroot grub2-install with --target=i386-pc.
func TestInstallGRUB_OpenEuler_BIOS(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/usr/sbin/grub2-install"] = []byte("#!/bin/sh\nexit 0\n")
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="openEuler"` + "\n")
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "openeuler"}}, "/mnt/root", "/dev/sda", ""); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	var sawBIOS bool
	for _, c := range exec.Calls() {
		if c.Name != "chroot" {
			continue
		}
		full := strings.Join(c.Args, " ")
		if !strings.Contains(full, "grub2-install") {
			continue
		}
		if !strings.Contains(full, "--target=i386-pc") {
			t.Fatalf("openEuler BIOS grub2-install missing --target=i386-pc: %v", c.Args)
		}
		if c.Args[len(c.Args)-1] != "/dev/sda" {
			t.Fatalf("openEuler BIOS grub2-install missing disk target: %v", c.Args)
		}
		sawBIOS = true
	}
	if !sawBIOS {
		t.Fatal("chroot grub2-install (BIOS) not invoked for openEuler")
	}
}

// efiLabelFor returns "openEuler" for both "openeuler" and "openEuler" boot IDs.
func TestEfiLabelFor_OpenEuler(t *testing.T) {
	if label := efiLabelFor("openEuler"); label != "openEuler" {
		t.Fatalf("efiLabelFor(openEuler) = %q, want openEuler", label)
	}
	if label := efiLabelFor("openeuler"); label != "openEuler" {
		t.Fatalf("efiLabelFor(openeuler) = %q, want openEuler", label)
	}
}

// registerEFIBootEntryRHEL finds the EFI dir at /boot/EFI/<id> when the
// ESP is mounted at /mnt/root/boot (openEuler layout), not just /boot/efi.
func TestRegisterEFIBootEntryRHEL_OpenEuler_BootMountPoint(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="openEuler"` + "\n")
	// openEuler layout: ESP at /boot, EFI files at /boot/EFI/openEuler/.
	fs.dirs["/mnt/root/boot/EFI/openEuler"] = true
	fs.files["/mnt/root/boot/EFI/openEuler/grubx64.efi"] = []byte("grub")
	fs.files["/proc/mounts"] = []byte("/dev/sda1 /mnt/root/boot vfat rw 0 0\n")
	deps := Deps{Exec: exec, FS: fs}

	err := registerEFIBootEntryRHEL(context.Background(), deps, "/mnt/root", "/dev/sda", "openEuler")
	if err != nil {
		t.Fatalf("expected EFI dir at /boot/EFI/openEuler to be found, got err: %v", err)
	}

	// Verify efibootmgr was called with the correct loader.
	var sawCreate bool
	for _, c := range exec.Calls() {
		if c.Name == "efibootmgr" && hasArg(c.Args, "--create") {
			sawCreate = true
			if !hasFlagValue(c.Args, "--loader", `\EFI\openEuler\grubx64.efi`) {
				t.Fatalf("loader should be \\EFI\\openEuler\\grubx64.efi, got %v", c.Args)
			}
			if !hasFlagValue(c.Args, "--label", "openEuler") {
				t.Fatalf("label should be openEuler, got %v", c.Args)
			}
		}
	}
	if !sawCreate {
		t.Fatal("efibootmgr --create was not invoked")
	}
}

// ---- SELinux / kdump --------------------------------------------------------

func TestDisableSELinuxIfEnforcing_FlipsToDisabled(t *testing.T) {
	fs := newMockFS()
	_ = fs.MkdirAll("/mnt/etc/selinux", 0o755)
	_ = fs.WriteFile("/mnt/etc/selinux/config",
		[]byte("# SELinux config\nSELINUX=enforcing\nSELINUXTYPE=targeted\n"), 0o644)
	deps := Deps{FS: fs, Logger: testLogger(t)}

	disableSELinuxIfEnforcing(deps, "/mnt")

	data, _ := fs.ReadFile("/mnt/etc/selinux/config")
	if !strings.Contains(string(data), "SELINUX=disabled") {
		t.Fatalf("expected SELINUX=disabled, got: %s", string(data))
	}
	if strings.Contains(string(data), "SELINUX=enforcing") {
		t.Fatalf("enforcing should be replaced, got: %s", string(data))
	}
}

func TestDisableSELinuxIfEnforcing_Idempotent(t *testing.T) {
	fs := newMockFS()
	_ = fs.MkdirAll("/mnt/etc/selinux", 0o755)
	_ = fs.WriteFile("/mnt/etc/selinux/config",
		[]byte("SELINUX=disabled\n"), 0o644)
	deps := Deps{FS: fs, Logger: testLogger(t)}

	disableSELinuxIfEnforcing(deps, "/mnt")

	data, _ := fs.ReadFile("/mnt/etc/selinux/config")
	// Should be unchanged.
	if string(data) != "SELINUX=disabled\n" {
		t.Fatalf("expected no change, got: %s", string(data))
	}
}

func TestMaskKdumpService_CreatesSymlinkToDevNull(t *testing.T) {
	fs := newMockFS()
	deps := Deps{FS: fs, Exec: newMockExec(), Logger: testLogger(t)}

	maskKdumpService(context.Background(), deps, "/mnt")

	target := "/mnt/etc/systemd/system/kdump.service"
	data, ok := fs.files[target]
	if !ok {
		t.Fatal("kdump.service mask symlink not created")
	}
	if !strings.HasPrefix(string(data), "symlink:") || !strings.HasSuffix(string(data), "/dev/null") {
		t.Fatalf("expected symlink to /dev/null, got: %s", string(data))
	}
}

func TestMaskKdumpService_Idempotent(t *testing.T) {
	fs := newMockFS()
	_ = fs.MkdirAll("/mnt/etc/systemd/system", 0o755)
	_ = fs.Symlink("/dev/null", "/mnt/etc/systemd/system/kdump.service")
	deps := Deps{FS: fs, Exec: newMockExec(), Logger: testLogger(t)}

	// Should not error or change the existing symlink.
	maskKdumpService(context.Background(), deps, "/mnt")

	data, _ := fs.ReadFile("/mnt/etc/systemd/system/kdump.service")
	if !strings.HasSuffix(string(data), "/dev/null") {
		t.Fatalf("symlink should still point to /dev/null, got: %s", string(data))
	}
}

func TestSelinuxWillBeDisabled(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "enforcing", body: "SELINUX=enforcing\n", want: false},
		{name: "permissive", body: "SELINUX=permissive\n", want: false},
		{name: "disabled", body: "SELINUX=disabled\n", want: true},
		{name: "missing", body: "", want: true}, // no file → effectively disabled
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newMockFS()
			_ = fs.MkdirAll("/mnt/etc/selinux", 0o755)
			if tc.body != "" {
				_ = fs.WriteFile("/mnt/etc/selinux/config", []byte(tc.body), 0o644)
			}
			deps := Deps{FS: fs}
			got := selinuxWillBeDisabled(deps, "/mnt")
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestFixRHELGrubCmdline_ReplacesSerialConsole(t *testing.T) {
	fs := newMockFS()
	_ = fs.MkdirAll("/mnt/etc/default", 0o755)
	_ = fs.WriteFile("/mnt/etc/default/grub", []byte(`GRUB_DEFAULT=saved
GRUB_CMDLINE_LINUX_DEFAULT="console=ttyS0,115200n8 no_timer_check crashkernel=1G-4G:192M"
GRUB_TIMEOUT=1
`), 0o644)
	exec := newMockExec()
	deps := Deps{FS: fs, Exec: exec, Logger: testLogger(t)}

	fixRHELGrubCmdline(context.Background(), deps, "/mnt")

	data, _ := fs.ReadFile("/mnt/etc/default/grub")
	if strings.Contains(string(data), "ttyS0") {
		t.Fatalf("ttyS0 should be replaced, got: %s", string(data))
	}
	if !strings.Contains(string(data), "console=tty0") {
		t.Fatalf("console=tty0 should be present, got: %s", string(data))
	}
	if !strings.Contains(string(data), "nomodeset") {
		t.Fatalf("nomodeset should be present, got: %s", string(data))
	}
}

func TestFixRHELGrubCmdline_FallbackToBLSEdit(t *testing.T) {
	fs := newMockFS()
	_ = fs.MkdirAll("/mnt/etc/default", 0o755)
	_ = fs.MkdirAll("/mnt/boot/loader/entries", 0o755)
	_ = fs.WriteFile("/mnt/etc/default/grub",
		[]byte(`GRUB_CMDLINE_LINUX_DEFAULT="console=ttyS0,115200n8 no_timer_check"`), 0o644)
	_ = fs.WriteFile("/mnt/boot/loader/entries/abc-6.12.conf",
		[]byte(`title Rocky
linux /vmlinuz-6.12
initrd /initramfs-6.12.img
options console=ttyS0,115200n8 no_timer_check root=UUID=abc
`), 0o644)
	// mockExec returns Default which is nil/nil — but make grubby "fail"
	// by setting a non-zero default error so the fallback path triggers.
	exec := newMockExec()
	exec.Default.Err = fmt.Errorf("grubby: command not found")
	// Also set On["chroot"] explicitly to be safe (Default alone should
	// suffice given mockExec's lookup, but make it bulletproof).
	exec.On["chroot"] = mockExecResult{Err: fmt.Errorf("grubby: command not found")}
	// `find` in the fallback path returns the list of BLS entries.
	exec.On["find"] = mockExecResult{Out: []byte("/mnt/boot/loader/entries/abc-6.12.conf")}
	deps := Deps{FS: fs, Exec: exec, Logger: testLogger(t)}

	fixRHELGrubCmdline(context.Background(), deps, "/mnt")

	data, _ := fs.ReadFile("/mnt/boot/loader/entries/abc-6.12.conf")
	if strings.Contains(string(data), "ttyS0") {
		t.Fatalf("BLS entry should not contain ttyS0, got: %s", string(data))
	}
	if !strings.Contains(string(data), "console=tty0") {
		t.Fatalf("BLS entry should have console=tty0, got: %s", string(data))
	}
	if !strings.Contains(string(data), "nomodeset") {
		t.Fatalf("BLS entry should have nomodeset, got: %s", string(data))
	}
}

// TestSetDefaultBootKernelToDriverComplete_PicksNewerKernelWithDrivers sets
// up two kernel version directories: an older one with NO megaraid_sas.ko
// (simulating a stripped cloud-image kernel that dnf failed to backfill) and
// a newer one with all critical drivers present (simulating a freshly-pulled
// kernel via `dnf install kernel-modules`). The function must pick the newer
// kernel and call `grubby --set-default=/boot/vmlinuz-<newer>`.
func TestSetDefaultBootKernelToDriverComplete_PicksNewerKernelWithDrivers(t *testing.T) {
	tmp := t.TempDir()
	oldKver := "6.12.0-211.16.1.el10_2.0.1.x86_64"
	newKver := "6.12.0-211.22.1.el10_2.x86_64"
	// Create /lib/modules dirs in the temp dir (listKernelVersions uses
	// os.ReadDir, so these need to be real directories).
	oldDir := filepath.Join(tmp, "lib", "modules", oldKver)
	newDir := filepath.Join(tmp, "lib", "modules", newKver)
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create the actual driver files only under the NEW kernel.
	for _, drv := range []string{"megaraid_sas", "mpt3sas", "hpsa", "aacraid", "smartpqi", "vfat"} {
		path := filepath.Join(newDir, drv+".ko.xz")
		if err := os.WriteFile(path, []byte("dummy"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	exec := newMockExec()
	// `find` is invoked once per (kernel, driver) pair. Use OnFull to
	// match on the specific modDir argument so the OLD kernel returns no
	// matches while the NEW kernel returns the file path.
	exec.OnFunc = func(name string, args []string) {
		// No-op; we use On/OnFull below.
		_ = name
		_ = args
	}
	// Helper: when find is invoked, run the actual filesystem lookup
	// against the temp dir so the mock reflects real on-disk state.
	exec.OnFull = map[string]mockExecResult{}
	// We can't easily pre-compute every (modDir, driver) combo into
	// OnFull keys because we don't know the temp dir path up front.
	// Instead, override Run via a wrapper.
	wrapped := &findPassthroughExec{inner: exec, root: tmp}

	deps := Deps{Exec: wrapped, FS: newMockFS(), Logger: testLogger(t)}
	setDefaultBootKernelToDriverComplete(context.Background(), deps, tmp)

	// Verify grubby was called with --set-default=/boot/vmlinuz-<newKver>.
	wantArg := "--set-default=/boot/vmlinuz-" + newKver
	found := false
	for _, c := range exec.Calls() {
		if c.Name == "chroot" {
			for _, a := range c.Args {
				if a == wantArg {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("expected chroot grubby %s, calls were: %+v", wantArg, exec.Calls())
	}
}

// findPassthroughExec delegates to a real `find` invocation against the
// temp dir root (so the test's real on-disk .ko files drive the scoring),
// while every other command goes to the mock.
type findPassthroughExec struct {
	inner *mockExec
	root  string
}

func (f *findPassthroughExec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "find" && len(args) >= 1 && strings.HasPrefix(args[0], f.root) {
		// Run a real `find` against the temp dir.
		cmdArgs := append([]string{}, args...)
		out, err := f.realFind(ctx, cmdArgs)
		if err == nil {
			return out, nil
		}
		return nil, err
	}
	return f.inner.Run(ctx, name, args...)
}

func (f *findPassthroughExec) RunPipe(ctx context.Context, stdin io.Reader, name string, args ...string) error {
	return f.inner.RunPipe(ctx, stdin, name, args...)
}

func (f *findPassthroughExec) realFind(_ context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// TestSetDefaultBootKernelToDriverComplete_GrubbyFailsFallsBackToGrubenv
// verifies the grubenv write path when grubby is unavailable.
func TestSetDefaultBootKernelToDriverComplete_GrubbyFailsFallsBackToGrubenv(t *testing.T) {
	tmp := t.TempDir()
	kver := "6.12.0-211.22.1.el10_2.x86_64"
	modDir := filepath.Join(tmp, "lib", "modules", kver)
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "megaraid_sas.ko.xz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create the grubenv file we expect the function to edit.
	grubenvDir := filepath.Join(tmp, "boot", "grub2")
	if err := os.MkdirAll(grubenvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	grubenvPath := filepath.Join(grubenvDir, "grubenv")
	initial := "# GRUB Environment Block\nsaved_entry=0\n### END /etc/grub.d/00_header ###\n"
	if err := os.WriteFile(grubenvPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	// mockFS reads from in-memory; mirror the grubenv file into it so the
	// fallback write succeeds.
	fs := newMockFS()
	_ = fs.MkdirAll(grubenvDir, 0o755)
	_ = fs.WriteFile(grubenvPath, []byte(initial), 0o644)

	exec := newMockExec()
	// Make grubby fail so we hit the fallback.
	exec.On["chroot"] = mockExecResult{Err: fmt.Errorf("grubby: not found")}
	wrapped := &findPassthroughExec{inner: exec, root: tmp}

	deps := Deps{Exec: wrapped, FS: fs, Logger: testLogger(t)}
	setDefaultBootKernelToDriverComplete(context.Background(), deps, tmp)

	data, err := fs.ReadFile(grubenvPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "saved_entry="+kver) {
		t.Fatalf("expected saved_entry=%s in grubenv, got: %s", kver, string(data))
	}
}
