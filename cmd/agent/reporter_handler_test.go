package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"metalkit/internal/installer"
)

// fakeReporter captures Log calls for assertion.
type fakeReporter struct {
	mu      sync.Mutex
	entries []logEntry
	stages  []string
}

func (f *fakeReporter) Stage(_ context.Context, s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stages = append(f.stages, s)
	return nil
}
func (f *fakeReporter) Log(_ context.Context, level, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, logEntry{level: level, message: msg})
	return nil
}
func (f *fakeReporter) Succeed(_ context.Context) error { return nil }
func (f *fakeReporter) Fail(_ context.Context, _ string) error { return nil }

func (f *fakeReporter) snapshot() []logEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]logEntry, len(f.entries))
	copy(out, f.entries)
	return out
}

func TestReporterHandler_ForwardsToReporter(t *testing.T) {
	rep := &fakeReporter{}
	inner := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})
	rh := newReporterHandler(inner, rep, "job-1")
	logger := slog.New(rh)

	logger.Info("starting install", "image", "rocky10", "size_gb", 10)
	logger.Warn("driver missing", "driver", "megaraid_sas")
	logger.Error("dracut failed", "err", io.ErrUnexpectedEOF)

	rh.Flush()
	// Allow worker goroutines a moment to finish in-flight POSTs.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(rep.snapshot()) >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	entries := rep.snapshot()
	if len(entries) < 3 {
		t.Fatalf("expected >= 3 forwarded entries, got %d: %+v", len(entries), entries)
	}
	// Each entry should carry the message and the level.
	wantMsgs := []string{"starting install", "driver missing", "dracut failed"}
	for i, want := range wantMsgs {
		if !strings.Contains(entries[i].message, want) {
			t.Errorf("entry %d: expected message %q, got %q", i, want, entries[i].message)
		}
	}
	if entries[0].level != "info" {
		t.Errorf("entry 0 level: expected info, got %s", entries[0].level)
	}
	if entries[1].level != "warn" {
		t.Errorf("entry 1 level: expected warn, got %s", entries[1].level)
	}
	if entries[2].level != "error" {
		t.Errorf("entry 2 level: expected error, got %s", entries[2].level)
	}
	// Attrs should be appended in [key=val] form.
	if !strings.Contains(entries[0].message, "[image=rocky10 size_gb=10]") {
		t.Errorf("entry 0 should contain attrs, got: %s", entries[0].message)
	}
}

func TestReporterHandler_NilReporterDoesNotPanic(t *testing.T) {
	inner := slog.NewJSONHandler(io.Discard, nil)
	rh := newReporterHandler(inner, nil, "")
	logger := slog.New(rh)

	// Should not panic.
	logger.Info("hello", "k", "v")
	rh.Flush()
}

func TestReporterHandler_LevelMapping(t *testing.T) {
	cases := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "debug"},
		{slog.LevelInfo, "info"},
		{slog.LevelWarn, "warn"},
		{slog.LevelError, "error"},
		{slog.LevelError + 4, "error"},
		{slog.LevelDebug - 4, "debug"},
	}
	for _, tc := range cases {
		got := slogLevelToString(tc.level)
		if got != tc.want {
			t.Errorf("level %v: want %s got %s", tc.level, tc.want, got)
		}
	}
}

// Compile-time assertion that fakeReporter satisfies installer.Reporter.
var _ installer.Reporter = (*fakeReporter)(nil)
