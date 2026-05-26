package installer

import (
	"context"
	"strings"
	"testing"

	"metalkit/internal/bindings"
	"metalkit/internal/jobs"
	"metalkit/internal/profiles"
)

// happyHarness wires up enough mocks for a successful Run.
type happyHarness struct {
	exec     *mockExec
	fs       *mockFS
	dl       *mockDownloader
	disks    *mockDisks
	reporter *mockReporter
	deps     Deps
}

func newHappyHarness(t *testing.T) *happyHarness {
	exec := newMockExec()
	fs := newMockFS()
	// UEFI environment
	_ = fs.MkdirAll("/sys/firmware/efi/efivars", 0o755)

	// lsblk -lnpo for grow on /dev/sda
	exec.OnFull["lsblk -lnpo NAME,TYPE /dev/sda"] = mockExecResult{
		Out: []byte("/dev/sda disk\n/dev/sda1 part\n/dev/sda2 part\n"),
	}
	// blkid: sda2 is ext4 (root), sda1 is vfat (ESP)
	exec.OnFull["blkid -o value -s TYPE /dev/sda2"] = mockExecResult{Out: []byte("ext4")}
	exec.OnFull["blkid -o value -s TYPE /dev/sda1"] = mockExecResult{Out: []byte("vfat")}

	disks := &mockDisks{
		Disks: []Disk{
			{Name: "sda", DevPath: "/dev/sda", SizeBytes: 100 * 1e9, Transport: "sata"},
		},
	}
	dl := &mockDownloader{}
	rep := &mockReporter{}

	deps := Deps{
		Exec: exec, FS: fs, Downloader: dl, Disks: disks, Reporter: rep,
		BaseURL: "http://10.0.0.1:8080", WorkDir: t.TempDir(),
	}
	return &happyHarness{exec, fs, dl, disks, rep, deps}
}

func happySpec() jobs.InstallSpec {
	return jobs.InstallSpec{
		JobID:        "job-1",
		MachineUUID:  "abcd1234deadbeef",
		ImageBlobURL: "/api/v1/images/abc/blob",
		ImageSHA256:  "deadbeef",
		Profile: profiles.Profile{
			HostnameTemplate: "n",
			RootPasswordHash: "$6$s$" + strings.Repeat("a", 86),
			TargetDisk:       profiles.TargetDisk{Mode: "smallest"},
			Network:          profiles.NetworkConfig{Method: "dhcp", NICSelector: "auto"},
		},
		Binding: bindings.Binding{MachineUUID: "abcd1234deadbeef"},
	}
}

func TestRun_HappyPath(t *testing.T) {
	h := newHappyHarness(t)
	err := Run(context.Background(), h.deps, happySpec())
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !h.reporter.success {
		t.Fatal("Succeed not called")
	}
	if h.reporter.failed != "" {
		t.Fatalf("Fail was called with %q on success path", h.reporter.failed)
	}
	wantStages := []string{
		StageBootDetect, StageDiskPick, StageDownload,
		StageWrite, StageGrow, StageMount, StageSeed,
		StageGrubInstall, StageUnmount,
	}
	got := h.reporter.Stages()
	if len(got) != len(wantStages) {
		t.Fatalf("stage count: got %d want %d (got: %v)", len(got), len(wantStages), got)
	}
	for i, w := range wantStages {
		if got[i] != w {
			t.Fatalf("stage[%d]=%q want %q (all=%v)", i, got[i], w, got)
		}
	}
}

func TestRun_BIOS_FailsAtBootDetect(t *testing.T) {
	h := newHappyHarness(t)
	// Remove efivars
	delete(h.fs.dirs, "/sys/firmware/efi/efivars")
	err := Run(context.Background(), h.deps, happySpec())
	if err == nil {
		t.Fatal("BIOS environment must fail")
	}
	if h.reporter.failed == "" {
		t.Fatal("Fail not called on BIOS rejection")
	}
	if h.reporter.success {
		t.Fatal("Succeed must not be called on BIOS rejection")
	}
	if !strings.Contains(h.reporter.failed, "UEFI") {
		t.Fatalf("Fail message %q should mention UEFI", h.reporter.failed)
	}
	stages := h.reporter.Stages()
	if len(stages) != 1 || stages[0] != StageBootDetect {
		t.Fatalf("only boot-detect stage should have been emitted, got %v", stages)
	}
}

func TestRun_DownloadFailure_CallsFailNotSucceed(t *testing.T) {
	h := newHappyHarness(t)
	h.dl.Err = errString("network down")
	err := Run(context.Background(), h.deps, happySpec())
	if err == nil {
		t.Fatal("download failure must error")
	}
	if h.reporter.success {
		t.Fatal("Succeed must not fire on download failure")
	}
	if h.reporter.failed == "" || !strings.Contains(h.reporter.failed, "network down") {
		t.Fatalf("Fail message did not include downloader error: %q", h.reporter.failed)
	}
	// Stages emitted up to download.
	stages := h.reporter.Stages()
	wantAtLeast := []string{StageBootDetect, StageDiskPick, StageDownload}
	if len(stages) != len(wantAtLeast) {
		t.Fatalf("stage prefix wrong, got %v", stages)
	}
}

func TestRun_DiskNotFound(t *testing.T) {
	h := newHappyHarness(t)
	h.disks.Disks = nil // no candidates
	err := Run(context.Background(), h.deps, happySpec())
	if err == nil {
		t.Fatal("missing disks must error")
	}
	if !strings.Contains(h.reporter.failed, "disk not found") {
		t.Fatalf("Fail %q should mention 'disk not found'", h.reporter.failed)
	}
}

func TestRun_MissingDepsErrorsOut(t *testing.T) {
	err := Run(context.Background(), Deps{}, happySpec())
	if err == nil {
		t.Fatal("missing Reporter must error")
	}
}

func TestRun_IncompleteDeps(t *testing.T) {
	err := Run(context.Background(), Deps{Reporter: &mockReporter{}}, happySpec())
	if err == nil {
		t.Fatal("missing Exec/FS/Downloader/Disks must error")
	}
}

func TestRun_XFSPath_DefersGrow(t *testing.T) {
	h := newHappyHarness(t)
	// Override blkid to report xfs on /dev/sda2 (root).
	h.exec.OnFull["blkid -o value -s TYPE /dev/sda2"] = mockExecResult{Out: []byte("xfs")}
	err := Run(context.Background(), h.deps, happySpec())
	if err != nil {
		t.Fatalf("xfs install should succeed: %v", err)
	}
	// xfs_growfs should have been invoked on the mounted root.
	wantTarget := h.deps.WorkDir + "/rootfs"
	var sawXFS bool
	for _, c := range h.exec.Calls() {
		if c.Name == "xfs_growfs" {
			sawXFS = true
			if len(c.Args) < 1 || c.Args[0] != wantTarget {
				t.Fatalf("xfs_growfs target wrong: %v (want %s)", c.Args, wantTarget)
			}
		}
	}
	if !sawXFS {
		t.Fatal("xfs_growfs was not invoked")
	}
}
