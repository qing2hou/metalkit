package settings

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
	"testing"

	"metalkit/internal/config"
)

func newStorageTestAPI(t *testing.T, bootDir string) (*API, *Store) {
	t.Helper()
	s := newTestStore(t)
	cfg := &config.Config{ImagesDir: bootDir}
	return NewAPI(s, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))), s
}

func strToReader(s string) io.Reader { return strings.NewReader(s) }

func TestStorageGetDefaultsToBootCfg(t *testing.T) {
	a, _ := newStorageTestAPI(t, "/var/lib/metalkit/images")
	w := httptest.NewRecorder()
	a.getStorage(w, httptest.NewRequest(http.MethodGet, "/api/v1/settings/storage", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var got StorageSettings
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ImagesDir != "/var/lib/metalkit/images" {
		t.Errorf("images_dir=%q", got.ImagesDir)
	}
}

func TestStoragePutMigratesFiles(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "old")
	newDir := filepath.Join(root, "new")
	if err := os.MkdirAll(oldDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Two content-addressed blobs + one dotfile + one dir that must NOT move.
	for _, name := range []string{"aa11.raw", "bb22.qcow2"} {
		if err := os.WriteFile(filepath.Join(oldDir, name), []byte(name), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldDir, ".hidden"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(oldDir, ".tmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	a, _ := newStorageTestAPI(t, oldDir)
	body := `{"images_dir":"` + newDir + `"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/settings/storage", strToReader(body))
	a.putStorage(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp StorageSettingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.RestartRequired {
		t.Error("restart_required should be true after a dir change")
	}
	for _, name := range []string{"aa11.raw", "bb22.qcow2"} {
		if _, err := os.Stat(filepath.Join(newDir, name)); err != nil {
			t.Errorf("blob %s not migrated: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(oldDir, name)); err == nil {
			t.Errorf("blob %s still in old dir", name)
		}
	}
	// dot-prefixed entry points (.tmp scratch dir) stay behind.
	if _, err := os.Stat(filepath.Join(oldDir, ".tmp")); err != nil {
		t.Errorf(".tmp should stay in old dir: %v", err)
	}

	// GET now reports the override.
	w2 := httptest.NewRecorder()
	a.getStorage(w2, httptest.NewRequest(http.MethodGet, "/api/v1/settings/storage", nil))
	var got StorageSettings
	_ = json.Unmarshal(w2.Body.Bytes(), &got)
	if got.ImagesDir != newDir {
		t.Errorf("effective images_dir=%q want %q", got.ImagesDir, newDir)
	}
}

func TestStoragePutRejectsNestedAndRelative(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "old")
	if err := os.MkdirAll(oldDir, 0o750); err != nil {
		t.Fatal(err)
	}
	a, _ := newStorageTestAPI(t, oldDir)

	for _, bad := range []string{
		`{"images_dir":"relative/path"}`,
		`{"images_dir":"` + filepath.Join(oldDir, "inside") + `"}`,
		`{"images_dir":""}`,
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, "/api/v1/settings/storage", strToReader(bad))
		a.putStorage(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: code=%d want 400 (%s)", bad, w.Code, w.Body.String())
		}
	}
}

func TestStoragePutNoOpIsIdempotent(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "same")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	a, _ := newStorageTestAPI(t, dir)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/settings/storage", strToReader(`{"images_dir":"`+dir+`"}`))
	a.putStorage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp StorageSettingsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.RestartRequired {
		t.Error("no-op PUT must not require restart")
	}
}

func TestApplyOverridesImagesDir(t *testing.T) {
	root := t.TempDir()
	goodDir := filepath.Join(root, "good")
	cfg := &config.Config{ImagesDir: "/var/lib/metalkit/images"}
	_, s := newStorageTestAPI(t, cfg.ImagesDir)

	if err := s.Set(context.Background(), KeyStorageImagesDir, goodDir, "test"); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := ApplyOverridesToConfig(context.Background(), s, cfg, logger); err != nil {
		t.Fatal(err)
	}
	if cfg.ImagesDir != goodDir {
		t.Errorf("ImagesDir=%q want %q", cfg.ImagesDir, goodDir)
	}

	// A relative path override is ignored, config value stays.
	if err := s.Set(context.Background(), KeyStorageImagesDir, "not/absolute", "test"); err != nil {
		t.Fatal(err)
	}
	cfg2 := &config.Config{ImagesDir: "/var/lib/metalkit/images"}
	if err := ApplyOverridesToConfig(context.Background(), s, cfg2, logger); err != nil {
		t.Fatal(err)
	}
	if cfg2.ImagesDir != "/var/lib/metalkit/images" {
		t.Errorf("relative override should be ignored, got %q", cfg2.ImagesDir)
	}
}
