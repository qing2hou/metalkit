package installer

import (
	"context"
	"strings"
	"testing"
)

func TestInstallGRUB_HappyPath(t *testing.T) {
	exec := newMockExec()
	fs := newMockFS()
	deps := Deps{Exec: exec, FS: fs}

	if err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// We no longer write a GRUB cmdline overlay — cloud-init finds the seed
	// via /var/lib/cloud/seed/nocloud-net/ auto-discovery, not the cmdline.
	if _, ok := fs.files["/mnt/root/etc/default/grub.d/99-metalkit.cfg"]; ok {
		t.Fatal("99-metalkit.cfg overlay must not be written (seed lives in rootfs)")
	}

	// chroot grub-install + update-grub were called.
	var sawInstall, sawUpdate bool
	for _, c := range exec.Calls() {
		if c.Name != "chroot" {
			continue
		}
		if len(c.Args) < 2 || c.Args[0] != "/mnt/root" {
			t.Fatalf("chroot args[0] should be mnt root, got %v", c.Args)
		}
		switch c.Args[1] {
		case "grub-install":
			sawInstall = true
			full := strings.Join(c.Args, " ")
			if !strings.Contains(full, "--target=x86_64-efi") {
				t.Fatalf("grub-install missing UEFI target: %v", c.Args)
			}
			if !strings.Contains(full, "--efi-directory=/boot/efi") {
				t.Fatalf("grub-install missing efi-directory: %v", c.Args)
			}
			if !strings.Contains(full, "--bootloader-id=metalkit") {
				t.Fatalf("grub-install missing bootloader-id: %v", c.Args)
			}
		case "update-grub":
			sawUpdate = true
		}
	}
	if !sawInstall {
		t.Fatal("grub-install not invoked")
	}
	if !sawUpdate {
		t.Fatal("update-grub not invoked")
	}
}

func TestInstallGRUB_GrubInstallFailureSurfaces(t *testing.T) {
	exec := newMockExec()
	exec.On["chroot"] = mockExecResult{Err: errString("efibootmgr write failed")}
	deps := Deps{Exec: exec, FS: newMockFS()}
	err := InstallGRUB(context.Background(), deps, "/mnt/root", "/dev/sda")
	if err == nil {
		t.Fatal("grub-install failure must surface")
	}
}
