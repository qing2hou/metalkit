package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"metalkit/internal/inventory"
)

func TestReadCmdline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cmdline")
	if err := os.WriteFile(path, []byte("BOOT_IMAGE=/vmlinuz boot=live metalkit.url=http://10.0.0.1:8080 ip=dhcp"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readCmdline(path, "metalkit.url")
	if err != nil {
		t.Fatalf("readCmdline: %v", err)
	}
	if got != "http://10.0.0.1:8080" {
		t.Errorf("got %q", got)
	}
}

func TestReadCmdlineMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cmdline")
	if err := os.WriteFile(path, []byte("boot=live ip=dhcp"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readCmdline(path, "metalkit.url")
	if err == nil {
		t.Fatal("expected error for missing key")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err=%v, expected 'not found'", err)
	}
}

func TestReadCmdlineQuoted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cmdline")
	if err := os.WriteFile(path, []byte(`metalkit.url="http://10.0.0.1:8080"`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readCmdline(path, "metalkit.url")
	if err != nil {
		t.Fatalf("readCmdline: %v", err)
	}
	if got != "http://10.0.0.1:8080" {
		t.Errorf("got %q", got)
	}
}

func TestPostReportLoopSuccess(t *testing.T) {
	var count int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		if r.URL.Path != "/api/v1/report" || r.Method != http.MethodPost {
			http.Error(w, "wrong path/method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"uuid": "abc-123", "report_id": 1})
	}))
	t.Cleanup(ts.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &inventory.Report{SchemaVersion: inventory.SchemaVersion}
	uuid, err := postReportLoop(context.Background(), logger, ts.Client(), ts.URL, r)
	if err != nil {
		t.Fatalf("postReportLoop: %v", err)
	}
	if uuid != "abc-123" {
		t.Errorf("uuid=%q", uuid)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Errorf("calls=%d want 1", got)
	}
}

func TestPostReportLoopRetriesOn5xx(t *testing.T) {
	var count int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&count, 1)
		if n < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"uuid": "ok", "report_id": 1})
	}))
	t.Cleanup(ts.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &inventory.Report{SchemaVersion: inventory.SchemaVersion}

	// Use a context with timeout in case the loop deadlocks.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	uuid, err := postReportLoop(ctx, logger, ts.Client(), ts.URL, r)
	if err != nil {
		t.Fatalf("postReportLoop: %v", err)
	}
	if uuid != "ok" {
		t.Errorf("uuid=%q", uuid)
	}
	if got := atomic.LoadInt32(&count); got != 3 {
		t.Errorf("calls=%d want 3", got)
	}
}

func TestPostReportLoopFailsOn4xx(t *testing.T) {
	var count int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	t.Cleanup(ts.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &inventory.Report{SchemaVersion: inventory.SchemaVersion}
	_, err := postReportLoop(context.Background(), logger, ts.Client(), ts.URL, r)
	if err == nil {
		t.Fatal("expected 4xx to abort the loop")
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Errorf("calls=%d want 1 (no retry on 4xx)", got)
	}
}

func TestSendHeartbeat(t *testing.T) {
	var gotURL string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ts.Close)

	if err := sendHeartbeat(context.Background(), ts.Client(), ts.URL+"/api/v1/heartbeat/abc"); err != nil {
		t.Fatalf("sendHeartbeat: %v", err)
	}
	if gotURL != "/api/v1/heartbeat/abc" {
		t.Errorf("path=%q", gotURL)
	}
}

func TestSendHeartbeatErrorsOnNon204(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)

	err := sendHeartbeat(context.Background(), ts.Client(), ts.URL+"/api/v1/heartbeat/abc")
	if err == nil {
		t.Fatal("expected error on 404")
	}
}
