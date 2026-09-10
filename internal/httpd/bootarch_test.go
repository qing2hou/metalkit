package httpd

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientArch(t *testing.T) {
	cases := []struct {
		ua   string
		q    string
		want string
	}{
		{"iPXE/x86_64-efi", "", "amd64"},
		{"iPXE/arm64-efi", "", "arm64"},
		{"iPXE (aarch64)", "", "arm64"},
		{"", "arch=arm64", "arm64"},
		{"iPXE/arm64-efi", "arch=amd64", "amd64"}, // explicit wins
		{"curl/8", "", "amd64"},                   // legacy default
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/boot/ipxe?"+c.q, nil)
		r.Header.Set("User-Agent", c.ua)
		if got := clientArch(r); got != c.want {
			t.Errorf("ua=%q q=%q: arch=%s want %s", c.ua, c.q, got, c.want)
		}
	}
}

func TestIPXEScriptPerArch(t *testing.T) {
	body, err := renderIPXE("10.1.2.3", ":8080", "arm64")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "console=ttyAMA0,115200") {
		t.Errorf("arm64 script missing ttyAMA0: %s", body)
	}
	if !strings.Contains(body, "arch=arm64") {
		t.Errorf("arm64 script missing arch param: %s", body)
	}

	bodyX, _ := renderIPXE("10.1.2.3", ":8080", "")
	if !strings.Contains(bodyX, "console=ttyS0,115200") {
		t.Errorf("default script missing ttyS0: %s", bodyX)
	}
	if !strings.Contains(bodyX, "arch=amd64") {
		t.Errorf("default script missing arch param: %s", bodyX)
	}
}

func TestBootFileArchFallback(t *testing.T) {
	dir := t.TempDir()
	// arm64 tree present; amd64 absent → amd64 request falls back flat.
	armDir := filepath.Join(dir, "arm64")
	_ = writeFile(t, filepath.Join(armDir, "vmlinuz"), "arm-kernel")
	_ = writeFile(t, filepath.Join(dir, "vmlinuz"), "flat-kernel")

	s := &Server{cfg: Config{BootDir: dir, Logger: testLogger()}}
	h := s.bootFile("vmlinuz")

	// arm64 request gets the arch tree file.
	r := httptest.NewRequest(http.MethodGet, "/boot/vmlinuz?arch=arm64", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if !strings.Contains(w.Body.String(), "arm-kernel") {
		t.Errorf("arm64 request body = %q", w.Body.String())
	}

	// amd64 request falls back to the flat layout.
	r2 := httptest.NewRequest(http.MethodGet, "/boot/vmlinuz?arch=amd64", nil)
	w2 := httptest.NewRecorder()
	h(w2, r2)
	if !strings.Contains(w2.Body.String(), "flat-kernel") {
		t.Errorf("amd64 fallback body = %q", w2.Body.String())
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeFile(t *testing.T, path, content string) error {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
