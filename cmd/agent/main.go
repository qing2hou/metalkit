// Package main is the metalkit inventory agent. It runs inside the Debian
// live image, collects hardware facts via the internal/inventory/collect
// package, POSTs an initial report to the controller, then heartbeats every
// 30s for the life of the live boot.
//
// Configuration comes from the kernel cmdline (/proc/cmdline) — the
// controller injects "metalkit.url=http://<ip>:<port>" via the iPXE template.
// A -url flag is honored for development.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"metalkit/internal/agentclient"
	"metalkit/internal/installer"
	"metalkit/internal/inventory"
	"metalkit/internal/inventory/collect"
	"metalkit/internal/jobs"
)

// agentVersion is overridden at link time via -ldflags="-X main.agentVersion=...".
var agentVersion = "dev"

// reporterLogTimeout caps how long a single Reporter.Log call may block
// the slog handler. The controller is normally on the LAN so this is
// generous; if it's down we drop the log line rather than stall the
// install pipeline.
var reporterLogTimeout = 5 * time.Second

func main() {
	os.Exit(run())
}

func run() int {
	urlFlag := flag.String("url", "", "controller base URL (e.g. http://10.0.0.1:8080); overrides /proc/cmdline metalkit.url=")
	cmdlinePath := flag.String("cmdline", "/proc/cmdline", "kernel cmdline path (for testing)")
	heartbeat := flag.Duration("heartbeat", 30*time.Second, "heartbeat interval")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	baseURL := *urlFlag
	if baseURL == "" {
		v, err := readCmdline(*cmdlinePath, "metalkit.url")
		if err != nil {
			logger.Error("agent: no metalkit.url=", "err", err, "cmdline_path", *cmdlinePath)
			return 1
		}
		baseURL = v
	}
	baseURL = strings.TrimRight(baseURL, "/")
	logger.Info("agent starting", "version", agentVersion, "url", baseURL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Step 1: collect. Never fails hard — collectors accumulate soft errors.
	collectCtx, collectCancel := context.WithTimeout(ctx, 2*time.Minute)
	report, err := collect.All(collectCtx, agentVersion)
	collectCancel()
	if err != nil {
		logger.Error("agent: collect aborted", "err", err)
		return 1
	}
	if report.Machine.SMBIOSUUID == "" {
		logger.Error("agent: SMBIOS UUID empty — refusing to post (dmidecode failed?)",
			"collector_errors", report.Agent.Errors)
		return 1
	}
	logger.Info("agent: collection done",
		"uuid", report.Machine.SMBIOSUUID,
		"duration_ms", report.CollectionDurationMS,
		"collector_errors", len(report.Agent.Errors),
	)

	// Step 2: POST the report, with bounded retries until accepted.
	client := &http.Client{Timeout: 30 * time.Second}
	uuid, err := postReportLoop(ctx, logger, client, baseURL, report)
	if err != nil {
		logger.Error("agent: report post failed terminally", "err", err)
		return 1
	}
	logger.Info("agent: report accepted", "uuid", uuid)

	// Convert inventory NICs to installer NICInfo for bond slave MAC resolution.
	installerNICs := make([]installer.NICInfo, 0, len(report.NICs))
	for _, n := range report.NICs {
		if n.Name != "" && n.MAC != "" {
			installerNICs = append(installerNICs, installer.NICInfo{Name: n.Name, MAC: n.MAC})
		}
	}

	// Step 3: heartbeat + install loops run concurrently for the life of the
	// live boot. installLoop exits early after it completes (or fails) one job
	// — the orchestrator's finalize tick reboots the machine via BMC and the
	// kernel kills this process before heartbeatLoop would notice. On SIGINT
	// both loops exit via ctx.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		heartbeatLoop(ctx, logger, client, baseURL, uuid, *heartbeat)
	}()
	go func() {
		defer wg.Done()
		installLoop(ctx, logger, baseURL, uuid, installerNICs)
	}()
	wg.Wait()
	logger.Info("agent: shutdown")
	return 0
}

// readCmdline reads path and returns the first value associated with key.
// Looks for both bare ("key=v") and quoted ("key=\"v\"") forms.
func readCmdline(path, key string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	prefix := key + "="
	for _, tok := range strings.Fields(string(b)) {
		if strings.HasPrefix(tok, prefix) {
			v := strings.TrimPrefix(tok, prefix)
			v = strings.Trim(v, `"`)
			if v == "" {
				return "", fmt.Errorf("%s= present but empty", key)
			}
			return v, nil
		}
	}
	return "", fmt.Errorf("%s= not found in %s", key, path)
}

// postReportLoop POSTs the report and retries with exponential backoff while
// the controller is unreachable or returning 5xx. 4xx is a client error and
// is returned immediately. Returns the uuid the controller assigned.
func postReportLoop(ctx context.Context, logger *slog.Logger, client *http.Client, baseURL string, r *inventory.Report) (string, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("marshal report: %w", err)
	}
	url := baseURL + "/api/v1/report"
	delay := 2 * time.Second
	const maxDelay = 60 * time.Second
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		uuid, retry, err := postReportOnce(ctx, client, url, body)
		if err == nil {
			return uuid, nil
		}
		if !retry {
			return "", err
		}
		logger.Warn("agent: report POST failed, retrying", "attempt", attempt, "err", err, "delay", delay)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delay):
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func postReportOnce(ctx context.Context, client *http.Client, url string, body []byte) (uuid string, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", true, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return "", true, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		UUID     string `json:"uuid"`
		ReportID int64  `json:"report_id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", false, fmt.Errorf("decode response: %w (body=%s)", err, respBody)
	}
	if out.UUID == "" {
		return "", false, fmt.Errorf("empty uuid in response: %s", respBody)
	}
	return out.UUID, false, nil
}

// heartbeatLoop sends a POST /api/v1/heartbeat/{uuid} every interval until
// ctx is cancelled. Transient failures are logged at warn; the loop never
// gives up while the live image is running.
func heartbeatLoop(ctx context.Context, logger *slog.Logger, client *http.Client, baseURL, uuid string, interval time.Duration) {
	url := baseURL + "/api/v1/heartbeat/" + uuid
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := sendHeartbeat(ctx, client, url); err != nil {
				logger.Warn("agent: heartbeat failed", "err", err)
			}
		}
	}
}

func sendHeartbeat(ctx context.Context, client *http.Client, url string) error {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return fmt.Errorf("status %d", resp.StatusCode)
}

// installPollInterval is the cadence at which we ask the controller whether
// our machine has a pending install job. Cheap GET — 10s is comfortable.
const installPollInterval = 10 * time.Second

// agentReporter adapts an agentclient.Client (which addresses jobs by ID) to
// the installer.Reporter interface (which already knows its job). Created
// once per claimed job so installer code never has to think about which job
// it's reporting against.
type agentReporter struct {
	cli   *agentclient.Client
	jobID string
}

func (r *agentReporter) Stage(ctx context.Context, stage string) error {
	return r.cli.Stage(ctx, r.jobID, stage)
}

func (r *agentReporter) Log(ctx context.Context, level, message string) error {
	return r.cli.Log(ctx, r.jobID, level, message)
}

func (r *agentReporter) Succeed(ctx context.Context) error {
	return r.cli.Succeed(ctx, r.jobID)
}

func (r *agentReporter) Fail(ctx context.Context, errMsg string) error {
	return r.cli.Fail(ctx, r.jobID, errMsg)
}

// installLoop polls /api/v1/agent/jobs/current; when a job appears it claims
// the job, fetches its InstallSpec, and runs the installer pipeline. Exits
// after one install completes (success or fail) — a live boot installs at
// most once, the orchestrator's finalize tick reboots into the new system.
func installLoop(ctx context.Context, logger *slog.Logger, baseURL, uuid string, nics []installer.NICInfo) {
	cli := agentclient.New(baseURL, uuid, logger.With("component", "agentclient"))
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		job, err := cli.CurrentJob(ctx)
		switch {
		case errors.Is(err, agentclient.ErrNoCurrentJob):
			// nothing scheduled yet; keep polling
		case err != nil:
			logger.Warn("agent: poll current job", "err", err)
		default:
			done := handleJob(ctx, logger, cli, baseURL, uuid, job, nics)
			if done {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(installPollInterval):
		}
	}
}

// handleJob walks one job through claim → spec → install. Returns true when
// the caller should stop polling (terminal outcome or fatal pre-install
// error). Returns false to keep polling (e.g. transient claim failure where
// we expect the controller to settle).
func handleJob(ctx context.Context, logger *slog.Logger, cli *agentclient.Client, baseURL, uuid string, job *jobs.Job, nics []installer.NICInfo) bool {
	// If the job is already in a terminal status, bail — we should not have
	// gotten it from /current, but be defensive.
	switch job.Status {
	case "succeeded", "failed", "cancelled":
		logger.Info("agent: skipping terminal job", "job_id", job.ID, "status", job.Status)
		return true
	}

	// Claim is the pending → running transition. If we already restarted
	// mid-install (status=running) the controller will 409; treat that as
	// "already mine" and proceed to install. Other failures are transient.
	if job.Status == "pending" {
		if _, err := cli.Claim(ctx, job.ID); err != nil {
			logger.Warn("agent: claim failed", "job_id", job.ID, "err", err)
			return false
		}
		logger.Info("agent: claimed job", "job_id", job.ID)
	}

	spec, err := cli.Spec(ctx, job.ID)
	if err != nil {
		logger.Error("agent: fetch spec", "job_id", job.ID, "err", err)
		_ = cli.Fail(ctx, job.ID, fmt.Sprintf("fetch spec: %v", err))
		return true
	}

	// Wrap the stderr logger so every installer log line also lands in
	// the controller's job_logs table (visible on the web platform).
	// Without this, deps.Logger.* calls only reach the agent's stderr in
	// the live ISO tmpfs — lost on reboot, invisible to the operator.
	reporter := &agentReporter{cli: cli, jobID: job.ID}
	inner := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	rh := newReporterHandler(inner, reporter, job.ID)
	jobLogger := slog.New(rh).With("component", "installer", "job_id", job.ID)

	deps := installer.Deps{
		Exec: installer.OSExec{},
		FS:   installer.OSFS{},
		// No timeout: image downloads stream multi-GB. The control-plane
		// agentclient uses a separate timed client; this one is dedicated to
		// blob streaming via installer.HTTPDownloader.
		Downloader: installer.HTTPDownloader{Client: &http.Client{Timeout: 0}},
		Disks:      installer.LsblkDiskLister{},
		Reporter:   reporter,
		BaseURL:    baseURL,
		WorkDir:    "/tmp/metalkit-install",
		Logger:     jobLogger,
		NICs:       nics,
	}
	logger.Info("agent: starting install",
		"job_id", job.ID,
		"image_id", spec.ImageID,
		"profile_id", spec.Profile.ID,
		"machine_uuid", spec.MachineUUID)
	if err := installer.Run(ctx, deps, *spec); err != nil {
		logger.Error("agent: install failed", "job_id", job.ID, "err", err)
		rh.Flush()
		return true
	}
	logger.Info("agent: install succeeded; awaiting BMC reboot to disk", "job_id", job.ID)
	rh.Flush()
	return true
}
