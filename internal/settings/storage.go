package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// KeyStorageImagesDir overrides config.yaml's imagesDir. Like dhcp.interface,
// it cannot hot-reload — the images.Store is constructed once at startup —
// but unlike it, the PUT handler migrates existing content immediately so a
// restart lands on a fully-populated directory.
const KeyStorageImagesDir = "storage.images_dir"

// StorageSettings is the GET response and PUT request shape.
type StorageSettings struct {
	ImagesDir string `json:"images_dir"`
}

// StorageSettingsResponse layers the restart hint over the settings; dir
// changes always require a restart (the running Store keeps the old path).
type StorageSettingsResponse struct {
	StorageSettings
	RestartRequired bool `json:"restart_required"`
}

// effectiveStorage merges boot config with the settings-table override.
func (a *API) effectiveStorage(ctx context.Context) (StorageSettings, error) {
	out := StorageSettings{ImagesDir: a.bootCfg.ImagesDir}
	v, err := a.store.GetMany(ctx, []string{KeyStorageImagesDir})
	if err != nil {
		return out, err
	}
	if dir, ok := v[KeyStorageImagesDir]; ok && strings.TrimSpace(dir) != "" {
		out.ImagesDir = strings.TrimSpace(dir)
	}
	return out, nil
}

// getStorage reports the effective image storage directory.
func (a *API) getStorage(w http.ResponseWriter, r *http.Request) {
	s, err := a.effectiveStorage(r.Context())
	if err != nil {
		a.logger.Error("settings get storage", "err", err)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// putStorage validates and persists a new images directory, migrating any
// existing content-addressed image files (sha256.{raw,qcow2,...}) from the
// current directory. Migration failures roll back partially-moved files and
// reject the PUT so the controller keeps running on the old directory.
func (a *API) putStorage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var in StorageSettings
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	cur, err := a.effectiveStorage(r.Context())
	if err != nil {
		a.logger.Error("settings put storage: load current", "err", err)
		writeError(w, http.StatusInternalServerError, "fetch failed")
		return
	}

	newDir := strings.TrimSpace(in.ImagesDir)
	if newDir == "" {
		writeError(w, http.StatusBadRequest, "images_dir 不能为空")
		return
	}
	if !filepath.IsAbs(newDir) {
		writeError(w, http.StatusBadRequest, "images_dir 必须是绝对路径")
		return
	}

	// No-op change first: an idempotent re-PUT of the current effective dir
	// (exact same path after Clean) neither migrates nor requires a restart.
	if cur.ImagesDir != "" && filepath.Clean(newDir) == filepath.Clean(cur.ImagesDir) {
		writeJSON(w, http.StatusOK, StorageSettingsResponse{
			StorageSettings: cur, RestartRequired: false,
		})
		return
	}
	// Refuse paths that would recurse into themselves (the new dir nested
	// under the current one, or vice versa) — the migration walk would trip
	// over its own output.
	if curD := filepath.Clean(cur.ImagesDir); curD != "" {
		if strings.HasPrefix(newDir+string(filepath.Separator), curD+string(filepath.Separator)) ||
			strings.HasPrefix(curD+string(filepath.Separator), newDir+string(filepath.Separator)) {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("新目录不能与当前目录 %s 相互嵌套", curD))
			return
		}
	}

	// Create the target and verify writability before touching anything.
	if err := os.MkdirAll(newDir, 0o750); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("创建目录失败: %v", err))
		return
	}
	probe := filepath.Join(newDir, ".mk-write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("目录不可写: %v", err))
		return
	}
	_ = os.Remove(probe)

	moved, err := migrateImages(cur.ImagesDir, newDir)
	if err != nil {
		// migrateImages already rolled back; surface the reason.
		a.logger.Error("storage migration failed; rolled back", "from", cur.ImagesDir, "to", newDir, "err", err)
		writeError(w, http.StatusInternalServerError, "迁移失败已回滚: "+err.Error())
		return
	}

	if err := a.store.SetMany(r.Context(), map[string]string{KeyStorageImagesDir: newDir}, basicAuthUser(r)); err != nil {
		// DB failed after files moved — push them back so state stays coherent.
		if rbErr := rollbackImages(newDir, cur.ImagesDir, moved); rbErr != nil {
			a.logger.Error("storage rollback after db failure failed", "err", rbErr,
				"files_stuck_in", newDir)
		}
		a.logger.Error("settings put storage", "err", err)
		writeError(w, http.StatusInternalServerError, "save failed")
		return
	}
	a.logger.Info("storage settings updated", "user", basicAuthUser(r),
		"images_dir", newDir, "migrated_files", len(moved))

	writeJSON(w, http.StatusOK, StorageSettingsResponse{
		StorageSettings: StorageSettings{ImagesDir: newDir},
		RestartRequired: true,
	})
}

// migrateImages moves every content-addressed image file (sha256.ext) from
// src to dst. On any error it moves back what it already relocated and
// returns the error, leaving src exactly as it was. Upload scratch dirs
// (.tmp) are intentionally left behind: in-flight uploads are rare and their
// sessions die with the directory change anyway.
func migrateImages(src, dst string) ([]string, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", src, err)
	}
	var moved []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dst, e.Name())
		if err := moveFile(from, to); err != nil {
			_ = rollbackImages(dst, src, moved)
			return nil, fmt.Errorf("move %s: %w", e.Name(), err)
		}
		moved = append(moved, e.Name())
	}
	return moved, nil
}

// rollbackImages moves the named files back from dst to src.
func rollbackImages(dst, src string, files []string) error {
	if err := os.MkdirAll(src, 0o750); err != nil {
		return err
	}
	for _, name := range files {
		if err := moveFile(filepath.Join(dst, name), filepath.Join(src, name)); err != nil {
			return err
		}
	}
	return nil
}

// moveFile renames within a filesystem; across filesystems it falls back to
// copy + fsync + remove so a crash can't leave a truncated image that the
// DB still references (content addressing would hide the corruption until
// install time).
func moveFile(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	tmp := to + ".migpart"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		in.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		in.Close()
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		in.Close()
		out.Close()
		os.Remove(tmp)
		return err
	}
	in.Close()
	out.Close()
	if err := os.Rename(tmp, to); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(from)
}
