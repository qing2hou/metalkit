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
	if !strings.Contains(body, "/boot/arm64/vmlinuz") {
		t.Errorf("arm64 script missing path-style kernel URL: %s", body)
	}
	// fetch URL must stay query-free: live-boot's initramfs derives the
	// archive type from the extension suffix and ?arch= breaks the match.
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "fetch=") && strings.Contains(line, "?") {
			t.Errorf("fetch URL carries a query string: %s", line)
		}
	}

	bodyX, _ := renderIPXE("10.1.2.3", ":8080", "")
	if !strings.Contains(bodyX, "console=ttyS0,115200") {
		t.Errorf("default script missing ttyS0: %s", bodyX)
	}
	if !strings.Contains(bodyX, "/boot/amd64/filesystem.squashfs") {
		t.Errorf("default script missing path-style squashfs URL: %s", bodyX)
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

func TestBootArchPathHandler(t *testing.T) {
	dir := t.TempDir()
	amdDir := filepath.Join(dir, "amd64")
	armDir := filepath.Join(dir, "arm64")
	_ = writeFile(t, filepath.Join(amdDir, "filesystem.squashfs"), "amd64-squashfs")
	_ = writeFile(t, filepath.Join(armDir, "filesystem.squashfs"), "arm64-squashfs")

	s := &Server{cfg: Config{BootDir: dir, Logger: testLogger()}}

	// Path-style URL serves the arch tree without any query params.
	r := httptest.NewRequest(http.MethodGet, "/boot/amd64/filesystem.squashfs", nil)
	w := httptest.NewRecorder()
	s.bootArchHandler("amd64")(w, r)
	if !strings.Contains(w.Body.String(), "amd64-squashfs") {
		t.Errorf("amd64 path body = %q", w.Body.String())
	}

	// Unknown files under /boot/<arch>/ are 404, not flat fallback.
	r2 := httptest.NewRequest(http.MethodGet, "/boot/amd64/evil.sh", nil)
	w2 := httptest.NewRecorder()
	s.bootArchHandler("amd64")(w2, r2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("evil path code = %d, want 404", w2.Code)
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
