package httpd

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, bootDir string) *Server {
	t.Helper()
	s, err := New(Config{
		ListenAddr: ":8080",
		ServerIP:   "10.99.0.1",
		BootDir:    bootDir,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t, t.TempDir())
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body=%q want %q", string(body), "ok")
	}
}

func TestIPXEScript(t *testing.T) {
	s := newTestServer(t, t.TempDir())
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/boot/ipxe")
	if err != nil {
		t.Fatalf("GET /boot/ipxe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	bs := string(body)

	for _, want := range []string{
		"#!ipxe",
		"kernel http://",
		"boot=live",
		"fetch=http://10.99.0.1:8080/boot/filesystem.squashfs",
		"initrd http://",
		"boot\n",
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("ipxe body missing %q\nfull body:\n%s", want, bs)
		}
	}
}

func TestServeVmlinuz(t *testing.T) {
	bootDir := t.TempDir()
	want := []byte("FAKE-VMLINUZ-CONTENT-12345")
	if err := os.WriteFile(filepath.Join(bootDir, "vmlinuz"), want, 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, bootDir)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/boot/vmlinuz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(want) {
		t.Fatalf("body mismatch: got=%q want=%q", got, want)
	}
}

func TestUnknownPath(t *testing.T) {
	s := newTestServer(t, t.TempDir())
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/totally/unknown")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
}

func TestRangeRequest(t *testing.T) {
	bootDir := t.TempDir()
	full := []byte("0123456789abcdef")
	if err := os.WriteFile(filepath.Join(bootDir, "initrd.img"), full, 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, bootDir)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/boot/initrd.img", nil)
	req.Header.Set("Range", "bytes=0-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range req: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status=%d want 206", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "0123" {
		t.Fatalf("body=%q want %q", got, "0123")
	}
}

func TestConstructorRejectsNonIPv4(t *testing.T) {
	cases := []string{"example.com", "", "::1", "not-an-ip"}
	for _, in := range cases {
		_, err := New(Config{
			ListenAddr: ":8080",
			ServerIP:   in,
			BootDir:    t.TempDir(),
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err == nil {
			t.Errorf("New(ServerIP=%q): expected error, got nil", in)
			continue
		}
		if !strings.Contains(err.Error(), "IPv4") {
			t.Errorf("New(ServerIP=%q): error %q should mention IPv4", in, err)
		}
	}
}

func TestStartShutdown(t *testing.T) {
	bootDir := t.TempDir()
	s, err := New(Config{
		ListenAddr: "127.0.0.1:0", // OS-assigned port — but http.Server.Addr is what we use
		ServerIP:   "10.99.0.1",
		BootDir:    bootDir,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Use an explicit free port via httptest dance: easier to just bind :0 and test shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	// Give the goroutine a moment to bind.
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned err: %v", err)
	}
}

func TestDerivePortSuffix(t *testing.T) {
	cases := map[string]string{
		":8080":            ":8080",
		"0.0.0.0:8080":     ":8080",
		"127.0.0.1:1234":   ":1234",
		"[::1]:9000":       ":9000",
	}
	for in, want := range cases {
		got, err := derivePortSuffix(in)
		if err != nil {
			t.Errorf("derivePortSuffix(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("derivePortSuffix(%q)=%q want %q", in, got, want)
		}
	}
}
