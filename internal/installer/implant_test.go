package installer

import (
	"context"
	"strings"
	"testing"

	"metalkit/internal/bindings"
	"metalkit/internal/jobs"
	"metalkit/internal/profiles"
)

// implantDeps wires a Deps with the live monitor binary present (as the
// real live image provides via scripts/build-live.sh staging).
func implantDeps(fs *mockFS, exec *mockExec) Deps {
	fs.files[liveMonitorBIN] = []byte("MONITOR-BINARY")
	return Deps{
		Exec:       exec,
		FS:         fs,
		Downloader: &mockDownloader{},
		Disks:      &mockDisks{Disks: []Disk{{Name: "sda", DevPath: "/dev/sda", SizeBytes: 1000, Transport: "sata"}}},
		Reporter:   &mockReporter{},
		BaseURL:    "http://10.0.0.1:8080",
	}
}

// Agent-installed profile → binary + conf + unit + enable symlink land in
// the target rootfs.
func TestImplantMonitorFullLayout(t *testing.T) {
	fs, exe := newMockFS(), newMockExec()
	deps := implantDeps(fs, exe)
	mnt := "/mnt/root"
	if err := implantMonitor(context.Background(), deps, mnt, "http://10.0.0.1:8080/"); err != nil {
		t.Fatalf("implantMonitor: %v", err)
	}

	bin, err := fs.ReadFile(mnt + "/usr/local/bin/metalkit-monitor")
	if err != nil {
		t.Fatalf("monitor binary missing: %v", err)
	}
	if string(bin) != "MONITOR-BINARY" {
		t.Errorf("binary content = %q, want MONITOR-BINARY", bin)
	}

	conf, err := fs.ReadFile(mnt + "/etc/metalkit/monitor.conf")
	if err != nil {
		t.Fatalf("monitor.conf missing: %v", err)
	}
	if !strings.Contains(string(conf), "url=http://10.0.0.1:8080") {
		t.Errorf("conf missing trimmed url: %q", conf)
	}
	if !strings.Contains(string(conf), "interval=60") {
		t.Errorf("conf missing interval: %q", conf)
	}

	unit, err := fs.ReadFile(mnt + "/etc/systemd/system/metalkit-monitor.service")
	if err != nil {
		t.Fatalf("unit missing: %v", err)
	}
	for _, want := range []string{
		"ExecStart=/usr/local/bin/metalkit-monitor",
		"WantedBy=multi-user.target",
		"Restart=on-failure",
	} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit missing %q", want)
		}
	}

	link := mnt + "/etc/systemd/system/multi-user.target.wants/metalkit-monitor.service"
	data, err := fs.ReadFile(link)
	if err != nil {
		t.Fatalf("enable symlink missing: %v", err)
	}
	if !strings.Contains(string(data), "/etc/systemd/system/metalkit-monitor.service") {
		t.Errorf("symlink target wrong: %q", data)
	}
}

// Missing live binary → hard error naming the cause (a live image built
// without `make monitor` must fail the job, not ship an unmonitored system).
func TestImplantMonitorMissingBinary(t *testing.T) {
	fs, exe := newMockFS(), newMockExec()
	deps := implantDeps(fs, exe)
	delete(fs.files, liveMonitorBIN)
	err := implantMonitor(context.Background(), deps, "/mnt/root", "http://c:8080")
	if err == nil {
		t.Fatal("expected error for missing live monitor binary")
	}
	if !strings.Contains(err.Error(), "make monitor") {
		t.Errorf("error should hint at 'make monitor', got: %v", err)
	}
}

// Empty controller URL is a programming error (BaseURL always set by the
// agent main) — still surfaced, not silently skipped.
func TestImplantMonitorEmptyURL(t *testing.T) {
	fs, exe := newMockFS(), newMockExec()
	deps := implantDeps(fs, exe)
	if err := implantMonitor(context.Background(), deps, "/mnt/root", ""); err == nil {
		t.Fatal("expected error for empty controller URL")
	}
}

// The full Run pipeline honours spec.Profile.AgentInstalled: true implants
// into <workdir>/rootfs, false reports the stage but leaves the rootfs
// untouched.
func TestRunRespectsAgentInstalledFlag(t *testing.T) {
	newSpec := func(implant bool) jobs.InstallSpec {
		return jobs.InstallSpec{
			JobID:        "j1",
			MachineUUID:  "abcd1234deadbeef",
			ImageBlobURL: "/api/v1/images/i/blob",
			ImageSHA256:  "deadbeef",
			Profile: profiles.Profile{OSFamily: "ubuntu",
				HostnameTemplate: "n",
				RootPasswordHash: "$6$s$" + strings.Repeat("a", 86),
				TargetDisk:       profiles.TargetDisk{Mode: "smallest"},
				Network:          profiles.NetworkConfig{Method: "dhcp", NICSelector: "auto"},
				AgentInstalled:   implant,
			},
			Binding: bindings.Binding{MachineUUID: "abcd1234deadbeef"},
		}
	}

	// Implant on: monitor binary present in the mounted rootfs at the end.
	h := newHappyHarness(t)
	h.fs.files[liveMonitorBIN] = []byte("MONITOR-BINARY")
	if err := Run(context.Background(), h.deps, newSpec(true)); err != nil {
		t.Fatalf("Run with AgentInstalled: %v", err)
	}
	if _, err := h.fs.ReadFile(h.deps.WorkDir + "/rootfs/usr/local/bin/metalkit-monitor"); err != nil {
		t.Errorf("Run should implant the monitor: %v", err)
	}

	// Implant off: nothing in the target.
	h2 := newHappyHarness(t)
	h2.fs.files[liveMonitorBIN] = []byte("MONITOR-BINARY")
	if err := Run(context.Background(), h2.deps, newSpec(false)); err != nil {
		t.Fatalf("Run without AgentInstalled: %v", err)
	}
	if _, err := h2.fs.ReadFile(h2.deps.WorkDir + "/rootfs/usr/local/bin/metalkit-monitor"); err == nil {
		t.Error("Run must not implant the monitor when AgentInstalled is false")
	}
}
