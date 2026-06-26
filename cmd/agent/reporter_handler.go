// Package main: reporterHandler is a slog.Handler that fans out each log
// record to TWO sinks:
//
//  1. An underlying handler (typically slog.NewJSONHandler(os.Stderr)) —
//     keeps the existing agent stderr stream working for live debugging.
//  2. An installer.Reporter via an async batched sink — sends each line
//     to the controller via Reporter.Log so the web platform's job-log
//     view shows the FULL install trace, not just the handful of explicit
//     Reporter.Log calls in installer/*.go.
//
// Why: previously, the bulk of installer diagnostics (dracut stderr,
// grubby output, driver-missing warnings, dnf install progress, find
// results, efibootmgr -v dumps) were emitted via deps.Logger.Info/Warn
// which only landed on the agent's stderr in the live ISO tmpfs —
// rebooting lost them, and the web platform saw a sparse subset. This
// handler makes every deps.Logger call visible on the web platform
// without touching installer code.
//
// Batching: each log line is one HTTP POST in the controller's API, so a
// verbose install (dracut/dnf can emit hundreds of lines per second)
// would saturate the controller with sequential posts. We push records
// into a buffered channel and a background goroutine fans them out with
// bounded concurrency (5 in-flight POSTs). The channel is non-blocking
// for the slog caller — if the consumer falls behind, drops are
// preferred over stalling the install pipeline. main.go calls Flush()
// after installer.Run returns to drain the channel before exiting.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"metalkit/internal/installer"
)

// reporterHandler wraps an inner handler and forwards each record to a
// Reporter via an async batched sink.
type reporterHandler struct {
	inner    slog.Handler
	sink     *asyncLogSink
	attrsMu  sync.Mutex // guards WithAttrs/WithGroup builder below
}

// newReporterHandler returns a handler that writes to inner AND forwards
// to reporter. The reporter may be nil (used before any job is claimed);
// in that case only the inner handler fires.
func newReporterHandler(inner slog.Handler, reporter installer.Reporter, jobID string) *reporterHandler {
	h := &reporterHandler{inner: inner}
	if reporter != nil {
		h.sink = newAsyncLogSink(reporter, jobID)
	}
	return h
}

// Enabled defers to the inner handler — we want the same level filtering.
func (h *reporterHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return h.inner.Enabled(context.Background(), lvl)
}

// Handle formats the record (message + attrs) into a single line and
// forwards it to the async sink. The inner handler is called first so
// stderr output is not blocked by a slow controller.
func (h *reporterHandler) Handle(ctx context.Context, r slog.Record) error {
	if err := h.inner.Handle(ctx, r.Clone()); err != nil {
		return err
	}
	if h.sink == nil {
		return nil
	}
	line := formatRecord(r)
	level := slogLevelToString(r.Level)
	h.sink.submit(logEntry{level: level, message: line})
	return nil
}

// WithAttrs and WithGroup delegate to the inner handler so existing
// slog.With(...) semantics work transparently. The reporter fan-out
// receives records after attrs are baked in.
func (h *reporterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &reporterHandler{
		inner: h.inner.WithAttrs(attrs),
		sink:  h.sink, // shared — same job, same sink
	}
}

func (h *reporterHandler) WithGroup(name string) slog.Handler {
	return &reporterHandler{
		inner: h.inner.WithGroup(name),
		sink:  h.sink,
	}
}

// Flush blocks until all submitted log entries have been POSTed (or
// dropped due to a full channel / failed POST). Call after installer.Run
// returns so trailing logs land on the controller before the agent
// process exits. Safe to call multiple times; no-op if sink is nil.
func (h *reporterHandler) Flush() {
	if h.sink != nil {
		h.sink.flush()
	}
}

// logEntry is one queued log line awaiting POST.
type logEntry struct {
	level   string
	message string
}

// asyncLogSink consumes logEntry records from a buffered channel and
// POSTs them via Reporter.Log with bounded concurrency. Dropping on a
// full channel is intentional — install throughput must not be gated on
// controller latency.
type asyncLogSink struct {
	reporter installer.Reporter
	jobID    string
	ch       chan logEntry
	wg       sync.WaitGroup
	once     sync.Once
	stopCh   chan struct{}
}

const (
	asyncLogChannelCap = 1024
	asyncLogConcurrency = 5
)

func newAsyncLogSink(reporter installer.Reporter, jobID string) *asyncLogSink {
	s := &asyncLogSink{
		reporter: reporter,
		jobID:    jobID,
		ch:       make(chan logEntry, asyncLogChannelCap),
		stopCh:   make(chan struct{}),
	}
	// Bounded worker pool. Each worker pulls from ch and POSTs.
	for i := 0; i < asyncLogConcurrency; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	return s
}

func (s *asyncLogSink) submit(e logEntry) {
	select {
	case s.ch <- e:
	default:
		// Channel full — drop. Better to lose a log line than stall
		// the install. The same line is already on stderr.
	}
}

func (s *asyncLogSink) worker() {
	defer s.wg.Done()
	for {
		select {
		case e, ok := <-s.ch:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), reporterLogTimeout)
			_ = s.reporter.Log(ctx, e.level, e.message)
			cancel()
		case <-s.stopCh:
			return
		}
	}
}

// flush closes stopCh (so workers stop taking new work) and waits for
// in-flight POSTs to complete. Any records still in the channel are
// drained synchronously by the caller's goroutine — workers may have
// exited, but we don't want to lose buffered logs.
func (s *asyncLogSink) flush() {
	s.once.Do(func() {
		close(s.stopCh)
	})
	// Drain any buffered entries ourselves (workers may already be gone).
	for {
		select {
		case e, ok := <-s.ch:
			if !ok {
				goto wait
			}
			ctx, cancel := context.WithTimeout(context.Background(), reporterLogTimeout)
			_ = s.reporter.Log(ctx, e.level, e.message)
			cancel()
		default:
			goto wait
		}
	}
wait:
	s.wg.Wait()
}

// formatRecord turns a slog.Record into a single-line string suitable
// for storage in the job_logs table. Format:
//
//	<message> [key1=val1 key2=val2 ...]
//
// Attrs are appended key=value style (similar to slog's text handler but
// without the time/level prefix — those are stored in separate DB columns
// or implied by the log level on the platform). Long values are not
// truncated here; the store's AppendLog truncates at 4096 bytes.
func formatRecord(r slog.Record) string {
	var sb strings.Builder
	sb.WriteString(r.Message)
	first := true
	r.Attrs(func(a slog.Attr) bool {
		if first {
			sb.WriteString(" [")
			first = false
		} else {
			sb.WriteString(" ")
		}
		sb.WriteString(a.Key)
		sb.WriteString("=")
		sb.WriteString(attrValueString(a.Value))
		return true
	})
	if !first {
		sb.WriteString("]")
	}
	return sb.String()
}

// attrValueString renders a slog.Value to a compact string. Errors are
// rendered as err.Error(); strings are written as-is (no quoting); other
// types fall back to fmt.Sprint.
func attrValueString(v slog.Value) string {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return err.Error()
		}
		return fmt.Sprint(v.Any())
	default:
		return fmt.Sprint(v.Any())
	}
}

// slogLevelToString maps slog levels to the controller's accepted level
// strings (debug/info/warn/error). Anything below info becomes debug;
// anything between info and warn is info; etc.
func slogLevelToString(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
