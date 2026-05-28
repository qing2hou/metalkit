package installer

import (
	"context"
	"strings"
	"testing"
)

func TestInstallGRUB_UEFI_HappyPath(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
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

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", ""); err != nil {
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
	err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", "/boot/efi")
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

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", ""); err != nil {
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

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", ""); err != nil {
		t.Fatalf("install should succeed even when all update-grub methods fail: %v", err)
	}
}

func TestInstallGRUB_RHELGrub2Symlink(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	// Pretend the CentOS image has a /boot/grub2/grub.cfg already.
	fs.files["/mnt/root/boot/grub2/grub.cfg"] = []byte("set root=...")
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
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

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
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

// Rocky UEFI without an ESP EFI dir for the detected id is unrecoverable
// — the image didn't ship a bootloader we can register. Surface as error.
func TestInstallGRUB_RockyTarget_UEFI_MissingShimErrors(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/usr/sbin/grub2-install"] = []byte("#!/bin/sh\nexit 0\n")
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="rocky"` + "\n")
	// No /boot/efi/EFI/rocky at all.
	fs.files["/proc/mounts"] = []byte("/dev/sda2 /mnt/root/boot/efi vfat rw 0 0\n")
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", "/boot/efi"); err == nil {
		t.Fatal("expected error when ESP has no /EFI/rocky directory")
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

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/nvme0n1", "/boot/efi"); err != nil {
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

// Don't add a duplicate NVRAM entry on reinstall.
func TestInstallGRUB_RockyTarget_UEFI_SkipsCreateWhenEntryExists(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	fs.files["/mnt/root/usr/sbin/grub2-install"] = []byte("#!/bin/sh\nexit 0\n")
	fs.files["/mnt/root/etc/os-release"] = []byte(`ID="rocky"` + "\n")
	fs.dirs["/mnt/root/boot/efi/EFI/rocky"] = true
	fs.files["/mnt/root/boot/efi/EFI/rocky/shimx64.efi"] = []byte("shim")
	fs.files["/proc/mounts"] = []byte("/dev/sda2 /mnt/root/boot/efi vfat rw 0 0\n")
	// efibootmgr (no args) → list call returns a matching entry.
	exec.On["efibootmgr"] = mockExecResult{Out: []byte(
		"BootCurrent: 0001\n" +
			"Boot0001* Rocky Linux\tHD(2,GPT,...,0x1)/File(\\EFI\\rocky\\shimx64.efi)\n",
	)}
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	for _, c := range exec.Calls() {
		if c.Name == "efibootmgr" && hasArg(c.Args, "--create") {
			t.Fatal("efibootmgr --create should be skipped when matching entry already exists")
		}
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

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
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

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", ""); err != nil {
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

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
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

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda", "/boot/efi"); err != nil {
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
