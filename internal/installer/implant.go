// implant.go copies the metalkit monitor — a standalone metrics-only
// agent — from the live image into the freshly written rootfs when the
// profile (or per-binding override) asked for it.
//
// Why a copy at install time instead of baking it into the OS image: the
// monitor ships with the controller/live image, not with every cloud
// image in the catalog; keeping it out of the images lets one image serve
// both monitored and unmonitored machines, and the monitor version always
// matches the controller that receives its reports.
//
// What lands in the target system (all under /usr/local + /etc):
//
//	/usr/local/bin/metalkit-monitor            — the binary (from the live root)
//	/etc/metalkit/monitor.conf                 — controller URL + interval
//	/etc/systemd/system/metalkit-monitor.service
//	/etc/systemd/system/multi-user.target.wants/metalkit-monitor.service
//	                                          — enable symlink (ln -s, no chroot needed)
//
// The service is NOT started here — the target isn't running; first boot's
// systemd picks up the enable symlink and starts it.
package installer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Paths in the live environment (alive for the whole install; provided by
// stageLiveMonitorBIN, overridable for tests via Deps).
const (
	liveMonitorBIN = "/usr/local/bin/metalkit-monitor"

	targetMonitorBIN = "/usr/local/bin/metalkit-monitor"
	targetConfDir    = "/etc/metalkit"
	targetConfFile   = "/etc/metalkit/monitor.conf"
	targetUnitFile   = "/etc/systemd/system/metalkit-monitor.service"
	targetWantsDir   = "/etc/systemd/system/multi-user.target.wants"
)

// monitorIntervalSeconds is the sample cadence written into monitor.conf.
// Kept in sync with the controller's freshness window (monitor.latestWithin
// = 3× this) so a one-beat miss doesn't flap the UI badge.
const monitorIntervalSeconds = 60

// implantMonitor copies the live monitor binary + generated unit into the
// mounted rootfs. mntRoot is the target / as mounted by the mount stage.
//
// Failure policy: fatal. Unlike best-effort touches (log lines, autorelabel),
// the operator explicitly asked for the monitor; silently shipping a system
// without it would contradict the profile. A missing live binary means the
// live image was built before `make monitor` existed — better to fail the
// job with a clear message than produce an unmonitored system.
func implantMonitor(ctx context.Context, deps Deps, mntRoot, controllerURL string) error {
	if mntRoot == "" {
		return fmt.Errorf("install: implantMonitor: mntRoot is empty")
	}
	if controllerURL == "" {
		return fmt.Errorf("install: implantMonitor: controller URL is empty (agent BaseURL missing?)")
	}

	// Read the binary from the live root and write it into the target.
	// A plain cross-filesystem cp — deps.FS has no copy, and shelling out
	// to cp(1) would need the same two reads anyway.
	bin, err := deps.FS.ReadFile(liveMonitorBIN)
	if err != nil {
		return fmt.Errorf("install: implant: read %s: %w (live image built without 'make monitor'?)",
			liveMonitorBIN, err)
	}
	dst := filepath.Join(mntRoot, targetMonitorBIN)
	if err := deps.FS.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("install: implant: mkdir %s: %w", filepath.Dir(dst), err)
	}
	if err := deps.FS.WriteFile(dst, bin, 0o755); err != nil {
		return fmt.Errorf("install: implant: write %s: %w", dst, err)
	}

	// Config file: controller URL + interval. The monitor tolerates
	// comments; keep it operator-readable.
	confDir := filepath.Join(mntRoot, targetConfDir)
	if err := deps.FS.MkdirAll(confDir, 0o755); err != nil {
		return fmt.Errorf("install: implant: mkdir %s: %w", confDir, err)
	}
	conf := fmt.Sprintf("# Managed by metalkit installer\nurl=%s\ninterval=%d\n",
		strings.TrimRight(controllerURL, "/"), monitorIntervalSeconds)
	if err := deps.FS.WriteFile(filepath.Join(mntRoot, targetConfFile), []byte(conf), 0o644); err != nil {
		return fmt.Errorf("install: implant: write %s: %w", targetConfFile, err)
	}

	// systemd unit. Same shape as the live agent's unit (simple service,
	// restart on failure) — see live-image/config/includes.chroot/.../
	// metalkit-agent.service.
	unit := fmt.Sprintf(`[Unit]
Description=metalkit monitoring agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
# The monitor owns its retry policy (collect never fails; POSTs back off to
# the next tick). Restart covers the exit-early cases (no URL, no UUID).
Restart=on-failure
RestartSec=30
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, targetMonitorBIN)
	if err := deps.FS.MkdirAll(filepath.Join(mntRoot, "etc/systemd/system"), 0o755); err != nil {
		return fmt.Errorf("install: implant: mkdir systemd dir: %w", err)
	}
	if err := deps.FS.WriteFile(filepath.Join(mntRoot, targetUnitFile), []byte(unit), 0o644); err != nil {
		return fmt.Errorf("install: implant: write %s: %w", targetUnitFile, err)
	}

	// Enable: a symlink is exactly what `systemctl enable` creates, and
	// doing it by hand avoids a chroot + systemd-firstboot dance on a
	// rootfs that may be from any distro generation.
	wants := filepath.Join(mntRoot, targetWantsDir)
	if err := deps.FS.MkdirAll(wants, 0o755); err != nil {
		return fmt.Errorf("install: implant: mkdir %s: %w", wants, err)
	}
	link := filepath.Join(wants, "metalkit-monitor.service")
	if deps.FS.Exists(link) {
		_ = deps.FS.Remove(link)
	}
	if err := deps.FS.Symlink(targetUnitFile, link); err != nil {
		return fmt.Errorf("install: implant: symlink %s: %w", link, err)
	}

	if deps.Logger != nil {
		deps.Logger.Info("implant: monitor implanted",
			"bin", dst, "conf", filepath.Join(mntRoot, targetConfFile),
			"controller", controllerURL)
	}
	return nil
}
