// Common test helpers for the installer package. Hand-rolled mocks for
// Exec / FS / Downloader / DiskLister / Reporter.
package installer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// mockExec records calls; runs canned responses keyed on argv[0] (name)
// or, when set, a full argv string match.
type mockExec struct {
	mu        sync.Mutex
	calls     []mockExecCall
	pipeCalls []mockPipeCall
	// On returns canned output/error for a command. The key is the
	// program name (args[0]); if a more specific key is needed callers
	// can use the OnFull map with the full argv joined by " ".
	On      map[string]mockExecResult
	OnFull  map[string]mockExecResult
	Default mockExecResult
	// PipeOn matches by program name; if absent we return nil (success).
	PipeOn      map[string]error
	PipeCapture *bytes.Buffer // optional: copy stdin into here for inspection
	// OnFunc, if set, is called for every Run with the recorded name/args
	// before lookup; lets tests peek at side-effect state.
	OnFunc func(name string, args []string)
}

type mockExecCall struct {
	Name string
	Args []string
}

type mockPipeCall struct {
	Name string
	Args []string
}

type mockExecResult struct {
	Out []byte
	Err error
}

func newMockExec() *mockExec {
	return &mockExec{
		On:     map[string]mockExecResult{},
		OnFull: map[string]mockExecResult{},
		PipeOn: map[string]error{},
	}
}

func (m *mockExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	m.calls = append(m.calls, mockExecCall{Name: name, Args: append([]string{}, args...)})
	fn := m.OnFunc
	m.mu.Unlock()
	if fn != nil {
		fn(name, args)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	full := name
	for _, a := range args {
		full += " " + a
	}
	if r, ok := m.OnFull[full]; ok {
		return r.Out, r.Err
	}
	if r, ok := m.On[name]; ok {
		return r.Out, r.Err
	}
	return m.Default.Out, m.Default.Err
}

func (m *mockExec) RunPipe(_ context.Context, stdin io.Reader, name string, args ...string) error {
	m.mu.Lock()
	m.pipeCalls = append(m.pipeCalls, mockPipeCall{Name: name, Args: append([]string{}, args...)})
	cap := m.PipeCapture
	m.mu.Unlock()
	if cap != nil && stdin != nil {
		if _, err := io.Copy(cap, stdin); err != nil {
			return err
		}
	}
	if err, ok := m.PipeOn[name]; ok {
		return err
	}
	return nil
}

func (m *mockExec) Calls() []mockExecCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mockExecCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// mockFS is an in-memory FS.
type mockFS struct {
	mu      sync.Mutex
	files   map[string][]byte
	dirs    map[string]bool
	notADir map[string]bool // paths Exists==true but IsDir()==false
}

func newMockFS() *mockFS {
	return &mockFS{
		files:   map[string][]byte{},
		dirs:    map[string]bool{},
		notADir: map[string]bool{},
	}
}

func (m *mockFS) ReadFile(path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if data, ok := m.files[path]; ok {
		return append([]byte{}, data...), nil
	}
	return nil, os.ErrNotExist
}

func (m *mockFS) WriteFile(path string, data []byte, _ os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[path] = append([]byte{}, data...)
	m.dirs[filepath.Dir(path)] = true
	return nil
}

func (m *mockFS) MkdirAll(path string, _ os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[path] = true
	return nil
}

func (m *mockFS) Stat(path string) (os.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.notADir[path] {
		return mockFileInfo{name: filepath.Base(path), isDir: false}, nil
	}
	if m.dirs[path] {
		return mockFileInfo{name: filepath.Base(path), isDir: true}, nil
	}
	if data, ok := m.files[path]; ok {
		return mockFileInfo{name: filepath.Base(path), size: int64(len(data))}, nil
	}
	return nil, os.ErrNotExist
}

func (m *mockFS) Exists(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dirs[path] || m.notADir[path] {
		return true
	}
	_, ok := m.files[path]
	return ok
}

func (m *mockFS) Symlink(oldname, newname string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[newname] = []byte("symlink:" + oldname)
	m.dirs[filepath.Dir(newname)] = true
	return nil
}

func (m *mockFS) Remove(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, path)
	return nil
}

type mockFileInfo struct {
	name  string
	size  int64
	isDir bool
}

func (f mockFileInfo) Name() string       { return f.name }
func (f mockFileInfo) Size() int64        { return f.size }
func (f mockFileInfo) Mode() os.FileMode  { return 0 }
func (f mockFileInfo) ModTime() time.Time { return time.Time{} }
func (f mockFileInfo) IsDir() bool        { return f.isDir }
func (f mockFileInfo) Sys() any           { return nil }

// mockDownloader returns canned bytes or an error.
type mockDownloader struct {
	Body io.ReadCloser
	Err  error
}

type readCloserWrap struct {
	io.Reader
}

func (readCloserWrap) Close() error { return nil }

func (m *mockDownloader) Stream(_ context.Context, _ string, _ string) (io.ReadCloser, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	if m.Body != nil {
		return m.Body, nil
	}
	return readCloserWrap{bytes.NewReader([]byte("payload"))}, nil
}

// mockDisks returns a fixed list.
type mockDisks struct {
	Disks []Disk
	Err   error
}

func (m *mockDisks) List(_ context.Context) ([]Disk, error) {
	return m.Disks, m.Err
}

// mockReporter records all stage transitions and log lines.
type mockReporter struct {
	mu       sync.Mutex
	stages   []string
	logs     []string
	success  bool
	failed   string
	StageErr error
}

func (m *mockReporter) Stage(_ context.Context, s string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stages = append(m.stages, s)
	return m.StageErr
}

func (m *mockReporter) Log(_ context.Context, level, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(m.logs, level+":"+msg)
	return nil
}

func (m *mockReporter) Succeed(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.success = true
	return nil
}

func (m *mockReporter) Fail(_ context.Context, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed = msg
	return nil
}

func (m *mockReporter) Stages() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.stages))
	copy(out, m.stages)
	return out
}

// errReader returns the given error on every Read.
type errReader struct {
	err error
}

func (e errReader) Read(_ []byte) (int, error) { return 0, e.err }

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var _ = errors.New // keep errors imported for future use
