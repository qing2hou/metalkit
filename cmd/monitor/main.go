// Package main is the metalkit monitor — a standalone, single-purpose
// metrics agent that the installer implants into the installed OS when the
// profile/binding asks for it. It is deliberately NOT the install agent:
// no job polling, no disk writes, no cloud-init logic. It reads /proc and
// /sys, POSTs a sample every interval, and that is all. A bug here can
// never break an install, and the installed system carries no installer
// code.
//
// Configuration is a single INI-ish file, /etc/metalkit/monitor.conf:
//
//	url=http://10.0.0.1:8080     # controller base URL
//	interval=60                  # seconds between samples
//
// The installer writes it at implant time (see installer/implant.go); the
// systemd unit runs this binary with no arguments. Flags exist for manual
// testing and override the file.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"metalkit/internal/monitor"
)

// monitorVersion is overridden at link time via -ldflags="-X main.monitorVersion=...".
var monitorVersion = "dev"

const defaultConfPath = "/etc/metalkit/monitor.conf"

func main() {
	os.Exit(run())
}

func run() int {
	confFlag := flag.String("conf", defaultConfPath, "config file path")
	urlFlag := flag.String("url", "", "controller base URL (overrides conf file)")
	intervalFlag := flag.Duration("interval", 0, "sample interval (overrides conf file; default 60s)")
	uuidFlag := flag.String("uuid", "", "machine UUID override (default: SMBIOS UUID from sysfs)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg := readConf(*confFlag, logger)

	baseURL := *urlFlag
	if baseURL == "" {
		baseURL = cfg.url
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		logger.Error("monitor: no url — not installed by metalkit or conf missing", "conf", *confFlag)
		return 1
	}

	interval := *intervalFlag
	if interval <= 0 {
		interval = cfg.interval
	}
	if interval <= 0 {
		interval = 60 * time.Second
	}

	uuid := *uuidFlag
	if uuid == "" {
		uuid = monitor.SMBIOSUUID()
	}
	if uuid == "" {
		logger.Error("monitor: SMBIOS UUID unavailable (not on real hardware?) — refusing to report")
		return 1
	}

	logger.Info("monitor starting",
		"version", monitorVersion, "url", baseURL,
		"uuid", uuid, "interval", interval.String())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	reportLoop(ctx, logger, baseURL, uuid, interval)
	logger.Info("monitor: shutdown")
	return 0
}

// conf is the parsed monitor.conf. Everything optional except url.
type conf struct {
	url      string
	interval time.Duration
}

// readConf parses key=value lines. Missing file is not an error at this
// layer (the -url flag may still supply everything); unparsable values are
// logged and skipped so one bad line can't take the monitor down.
func readConf(path string, logger *slog.Logger) conf {
	var c conf
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("monitor: read conf failed", "err", err, "path", path)
		}
		return c
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "url":
			c.url = val
		case "interval":
			var secs time.Duration
			if d, err := time.ParseDuration(val); err == nil {
				secs = d
			} else if n, err2 := fmt.Sscanf(val, "%d", new(int)); err2 == nil && n == 1 {
				// bare integer = seconds
				var nSecs int
				fmt.Sscanf(val, "%d", &nSecs)
				secs = time.Duration(nSecs) * time.Second
			}
			if secs > 0 {
				c.interval = secs
			} else {
				logger.Warn("monitor: bad interval in conf, using default", "value", val)
			}
		default:
			// Unknown keys (future extensions) are ignored.
		}
	}
	return c
}

// reportLoop collects and POSTs one sample per interval until ctx is done.
// POST failures are logged and retried next tick — a monitoring agent must
// be more available-minded than the network it sits on. 4xx (we sent
// garbage the controller rejected) is also retried: the controller may
// upgrade and transiently reject a schema before catching up; wedging the
// agent would turn a deploy blip into a monitoring outage.
func reportLoop(ctx context.Context, logger *slog.Logger, baseURL, uuid string, interval time.Duration) {
	client := &http.Client{Timeout: 15 * time.Second}
	url := baseURL + "/api/v1/agent/metrics"
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		// Fire immediately on startup so a freshly implanted monitor shows
		// up without waiting a full interval.
		if err := reportOnce(ctx, logger, client, url, uuid); err != nil {
			logger.Warn("monitor: report failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func reportOnce(ctx context.Context, logger *slog.Logger, client *http.Client, url, uuid string) error {
	p := monitor.CollectPayload(monitor.OSFS{}, uuid)
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
