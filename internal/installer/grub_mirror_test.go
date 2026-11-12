package installer

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"metalkit/internal/jobs"
	"metalkit/internal/profiles"
)

func TestWriteChrootAPTMirrorUbuntuUsesTargetCodename(t *testing.T) {
	fs := newMockFS()
	fs.files["/mnt/root/etc/os-release"] = []byte("NAME=\"Ubuntu\"\nVERSION_CODENAME=jammy\n")
	deps := Deps{FS: fs}

	m := chrootAptMirrors[1] // aliyun
	if err := writeChrootAPTMirror(deps, "/mnt/root", true, m); err != nil {
		t.Fatalf("write ubuntu mirror: %v", err)
	}
	got := string(fs.files["/mnt/root/etc/apt/sources.list.d/ubuntu.sources"])
	for _, want := range []string{
		"URIs: " + m.ubuntu,
		"Suites: jammy jammy-updates jammy-backports",
		"Suites: jammy-security",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ubuntu.sources missing %q:\n%s", want, got)
		}
	}
}

func TestWriteChrootAPTMirrorDebian(t *testing.T) {
	fs := newMockFS()
	deps := Deps{FS: fs}
	m := chrootAptMirrors[2] // ustc

	if err := writeChrootAPTMirror(deps, "/mnt/root", false, m); err != nil {
		t.Fatalf("write debian mirror: %v", err)
	}
	if got := string(fs.files["/mnt/root/etc/apt/mirrors/debian.list"]); got != m.debian+"\n" {
		t.Errorf("debian.list = %q, want %q", got, m.debian+"\n")
	}
	if got := string(fs.files["/mnt/root/etc/apt/mirrors/debian-security.list"]); got != m.debianSec+"\n" {
		t.Errorf("debian-security.list = %q, want %q", got, m.debianSec+"\n")
	}
}

// mirrorProbeExec fails apt-get update while the TUNA URL is configured and
// succeeds once a fallback rewrite lands. It lets the test verify the retry
// order without mocking per-argv calls (the argv is identical every time).
type mirrorProbeExec struct {
	fs      *mockFS
	updates int
}

func (e *mirrorProbeExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "chroot" && len(args) >= 3 && args[1] == "apt-get" && args[2] == "update" {
		e.updates++
		src := string(e.fs.files["/mnt/root/etc/apt/sources.list.d/ubuntu.sources"])
		if strings.Contains(src, "tuna.tsinghua") {
			return []byte("403 Forbidden"), errors.New("exit status 100")
		}
		return nil, nil
	}
	return nil, nil
}

func (e *mirrorProbeExec) RunPipe(context.Context, io.Reader, string, ...string) error {
	return nil
}

func TestChrootAptUpdateFallsBackToNextMirror(t *testing.T) {
	fs := newMockFS()
	fs.files["/mnt/root/etc/os-release"] = []byte("VERSION_CODENAME=noble\n")
	exec := &mirrorProbeExec{fs: fs}
	deps := Deps{Exec: exec, FS: fs}

	ok := chrootAptUpdate(context.Background(), deps, jobs.InstallSpec{Profile: profiles.Profile{OSFamily: "ubuntu"}}, "/mnt/root")
	if !ok {
		t.Fatal("expected a fallback mirror to succeed")
	}
	if exec.updates != 2 {
		t.Fatalf("apt-get update calls = %d, want 2 (tuna fail + aliyun ok)", exec.updates)
	}
	got := string(fs.files["/mnt/root/etc/apt/sources.list.d/ubuntu.sources"])
	if !strings.Contains(got, "mirrors.aliyun.com") {
		t.Errorf("sources did not switch to the fallback mirror:\n%s", got)
	}
}

func TestUbuntuKernelPackages(t *testing.T) {
	exec := newMockExec()
	exec.OnFull["chroot /mnt/root apt-cache depends linux-image-generic"] = mockExecResult{Out: []byte(
		"linux-image-generic\n" +
			"  Depends: linux-image-6.8.0-149-generic\n" +
			"  Depends: linux-modules-extra-6.8.0-149-generic\n" +
			"  Depends: linux-firmware\n" +
			" |Depends: <linux-image>\n")}
	deps := Deps{Exec: exec}

	pkgs, err := ubuntuKernelPackages(context.Background(), deps, "/mnt/root")
	if err != nil {
		t.Fatalf("resolve kernel packages: %v", err)
	}
	want := []string{"linux-image-6.8.0-149-generic", "linux-modules-extra-6.8.0-149-generic"}
	if len(pkgs) != len(want) || pkgs[0] != want[0] || pkgs[1] != want[1] {
		t.Fatalf("packages = %v, want %v", pkgs, want)
	}
}
