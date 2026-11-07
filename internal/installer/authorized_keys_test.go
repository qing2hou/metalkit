package installer

import (
	"context"
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
