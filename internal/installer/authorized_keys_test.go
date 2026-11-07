package installer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"metalkit/internal/bindings"
	"metalkit/internal/jobs"
	"metalkit/internal/profiles"
)

// specSSHKeys builds a minimal DHCP ubuntu spec with the given SSH keys.
func specSSHKeys(keys []string) jobs.InstallSpec {
	return jobs.InstallSpec{
		JobID: "job-ssh",
		Profile: profiles.Profile{OSFamily: "ubuntu",
			HostnameTemplate: "n",
			RootPasswordHash: "$6$s$" + strings.Repeat("a", 86),
			Network:          profiles.NetworkConfig{Method: "dhcp", NICSelector: "auto"},
		},
		Binding:       bindings.Binding{MachineUUID: "abcd1234"},
		SSHPublicKeys: keys,
	}
}

// TestBuildSeed_SSHKeysInjected: keys on the spec land in
// /root/.ssh/authorized_keys with the exact lines, and nothing leaks into
// user-data (directory keys would apply to cloud-init's *default* user,
// not root — the file write is the whole injection path).
func TestBuildSeed_SSHKeysInjected(t *testing.T) {
	fs := newMockFS()
	deps := Deps{Exec: newMockExec(), FS: fs, WorkDir: "/tmp/test-install"}
	keys := []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIEeIM72OY68FIhL6i3KsXruAYy0C5matj1jacX/rDwd ops@metalkit",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIEeIM72OY68FIhL6i3KsXruAYy0C5matj1jacX/rDwd backup@metalkit",
	}

	if err := BuildSeed(context.Background(), deps, specSSHKeys(keys), "/mnt/root"); err != nil {
		t.Fatalf("BuildSeed: %v", err)
	}
	content, ok := fs.files["/mnt/root/root/.ssh/authorized_keys"]
	if !ok {
		t.Fatalf("authorized_keys not written; files=%v", fs.files)
	}
	if want := strings.Join(keys, "\n") + "\n"; string(content) != want {
		t.Fatalf("authorized_keys:\n  got:  %q\n  want: %q", content, want)
	}

	// user-data must stay byte-identical to the no-keys render: the SSH
	// injection must not perturb the cloud-init golden output.
	fs2 := newMockFS()
	deps2 := Deps{Exec: newMockExec(), FS: fs2, WorkDir: "/tmp/test-install"}
	_ = BuildSeed(context.Background(), deps2, specSSHKeys(nil), "/mnt/root")
	noKey, _ := fs.files["/mnt/root/var/lib/cloud/seed/nocloud-net/user-data"]
	drop, _ := fs2.files["/mnt/root/var/lib/cloud/seed/nocloud-net/user-data"]
	if string(noKey) != string(drop) {
		t.Fatalf("user-data changed when keys present:\n  with: %q\n  without: %q", noKey, drop)
	}
}

// TestBuildSeed_NoKeysNoFile: no key selection → no .ssh dir, no empty
// authorized_keys, nothing overwrites a pre-existing file.
func TestBuildSeed_NoKeysNoFile(t *testing.T) {
	fs := newMockFS()
	_ = fs.MkdirAll("/mnt/root/root/.ssh", 0o700)
	_ = fs.WriteFile("/mnt/root/root/.ssh/authorized_keys", []byte("from-the-image\n"), 0o600)
	deps := Deps{Exec: newMockExec(), FS: fs, WorkDir: "/tmp/test-install"}

	if err := BuildSeed(context.Background(), deps, specSSHKeys(nil), "/mnt/root"); err != nil {
		t.Fatalf("BuildSeed: %v", err)
	}
	// Pre-existing image-provided file survives untouched.
	if got, _ := fs.files["/mnt/root/root/.ssh/authorized_keys"]; string(got) != "from-the-image\n" {
		t.Fatalf("existing authorized_keys overwritten: %q", got)
	}
}

// TestBuildSeed_SSHKeys_RHELFamilyRelabelRootSSH: on RHEL-family rootfs the
// restorecon invocation must include /root/.ssh, and the file must already
// exist by then — writeAuthorizedKeys runs BEFORE the relabel block, so a
// freshly created authorized_keys gets correct ssh_home_t context instead
// of inheriting the live system's (labeled wrong under enforcing → sshd
// can't read it → key login silently fails with green job status).
// The mock can't observe ordering directly; we pin the observable contract
// (file written + /root/.ssh on the restorecon arg list) — the code comment
// at the call site documents the ordering invariant.
func TestBuildSeed_SSHKeys_RHELFamilyRelabelRootSSH(t *testing.T) {
	fs := newMockFS()
	_ = fs.MkdirAll("/mnt/root/etc", 0o755)
	_ = fs.MkdirAll("/mnt/root/etc/selinux", 0o755)
	_ = fs.WriteFile("/mnt/root/etc/os-release", []byte("ID=\"rocky\"\nVERSION_ID=\"9.3\"\n"), 0o644)
	_ = fs.WriteFile("/mnt/root/etc/selinux/config",
		[]byte("SELINUX=enforcing\nSELINUXTYPE=targeted\n"), 0o644)
	exec := newMockExec()
	deps := Deps{Exec: exec, FS: fs, WorkDir: "/tmp/test"}

	spec := specSSHKeys([]string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIEeIM72OY68FIhL6i3KsXruAYy0C5matj1jacX/rDwd ops@metalkit",
	})
	spec.Profile.OSFamily = "rhel"
	if err := BuildSeed(context.Background(), deps, spec, "/mnt/root"); err != nil {
		t.Fatalf("BuildSeed: %v", err)
	}
	if _, ok := fs.files["/mnt/root/root/.ssh/authorized_keys"]; !ok {
		t.Fatalf("authorized_keys missing on RHEL-family rootfs")
	}
	foundRootSSH := false
	for _, c := range exec.Calls() {
		if c.Name == "chroot" && len(c.Args) > 3 && c.Args[1] == "restorecon" {
			for _, a := range c.Args[2:] {
				if a == "/root/.ssh" {
					foundRootSSH = true
				}
			}
		}
	}
	if !foundRootSSH {
		t.Fatalf("restorecon args missing /root/.ssh: %v", exec.Calls())
	}
}

// TestBuildSeed_SSHKeysWriteFailureFailsJob: a failed authorized_keys write
// must fail the job — silently skipping it would produce a green-but-keyless
// install, contradicting the spec endpoint's fail-closed posture on dangling
// key references.
func TestBuildSeed_SSHKeysWriteFailureFailsJob(t *testing.T) {
	fs := newMockFS()
	fs.failWrites["/mnt/root/root/.ssh/authorized_keys"] = errors.New("read-only filesystem")
	deps := Deps{Exec: newMockExec(), FS: fs, WorkDir: "/tmp/test-install"}

	spec := specSSHKeys([]string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIEeIM72OY68FIhL6i3KsXruAYy0C5matj1jacX/rDwd ops@metalkit"})
	err := BuildSeed(context.Background(), deps, spec, "/mnt/root")
	if err == nil {
		t.Fatalf("BuildSeed succeeded despite authorized_keys write failure")
	}
	if !strings.Contains(err.Error(), "authorized_keys") {
		t.Fatalf("error should name authorized_keys: %v", err)
	}
}
