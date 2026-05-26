package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"
)

// On-disk layout under s.dir:
//
//	{dir}/                       — root, owned by metalkit (root)
//	{dir}/.tmp/                  — chunked-upload working dirs
//	{dir}/.tmp/{upload_id}/chunk-000001
//	{dir}/.tmp/{upload_id}/chunk-000002 ...
//	{dir}/{sha256}.{format}      — final, content-addressed images
//
// The filename is the sha256 hex digest plus a {format} suffix (.qcow2 / .raw)
// so an operator browsing the directory can tell file type at a glance.

const (
	tmpSubdir         = ".tmp"
	chunkFileMode     = 0o600
	finalFileMode     = 0o644
	staleUploadCutoff = 24 * time.Hour
)

func (s *Store) ensureDirs() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}
	if err := os.MkdirAll(filepath.Join(s.dir, tmpSubdir), 0o700); err != nil {
		return fmt.Errorf("mkdir tmp: %w", err)
	}
	return nil
}

// Dir returns the on-disk root. Exposed for tests.
func (s *Store) Dir() string { return s.dir }

// uploadDir is the temp directory for a single upload session's chunks.
func (s *Store) uploadDir(sessionID string) string {
	return filepath.Join(s.dir, tmpSubdir, sessionID)
}

// chunkPath is where chunk N (1-based) is stored under an upload session.
func (s *Store) chunkPath(sessionID string, n int) string {
	return filepath.Join(s.uploadDir(sessionID), fmt.Sprintf("chunk-%06d", n))
}

// FinalPath returns where the image with the given sha256 + format should live.
func (s *Store) FinalPath(sha, format string) string {
	return filepath.Join(s.dir, sha+"."+format)
}

// WriteChunk reads body into chunk N (1-based) of session, atomically. The
// caller-supplied expectedSHA must match the chunk bytes (else ErrSHAMismatch).
// Chunk size is validated against the session's ChunkSize: the last chunk may
// be shorter; all others must match exactly. Returns the number of bytes
// written (== len(body)).
//
// On success the store's uploaded_chunks high-water mark is bumped if n is
// greater than the current value (so out-of-order writes still record
// monotonic progress for UI).
func (s *Store) WriteChunk(ctx context.Context, sessionID string, n int, body io.Reader, expectedSHA string) (int64, error) {
	sess, err := s.GetUpload(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if n < 1 || n > sess.NumChunks {
		return 0, fmt.Errorf("images: chunk %d out of range [1,%d]", n, sess.NumChunks)
	}
	if expectedSHA != "" && !sha256RE.MatchString(expectedSHA) {
		return 0, errors.New("images: chunk sha256 must be 64 lowercase hex chars")
	}

	expectedLen := sess.ChunkSize
	if n == sess.NumChunks {
		// Last chunk may be partial.
		expectedLen = sess.TotalSize - sess.ChunkSize*int64(sess.NumChunks-1)
	}

	if err := os.MkdirAll(s.uploadDir(sessionID), 0o700); err != nil {
		return 0, fmt.Errorf("mkdir upload dir: %w", err)
	}

	// Stream into a temp file in the same directory, hash + size as we go,
	// then rename. Limiting the reader means a misbehaving client cannot
	// exhaust disk by sending more than the declared chunk size.
	limited := io.LimitReader(body, expectedLen+1)
	final := s.chunkPath(sessionID, n)
	tmp := final + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, chunkFileMode)
	if err != nil {
		return 0, fmt.Errorf("open chunk tmp: %w", err)
	}
	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, h), limited)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("write chunk: %w", err)
	}
	if written != expectedLen {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("images: chunk %d length %d != expected %d", n, written, expectedLen)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if expectedSHA != "" && got != expectedSHA {
		_ = os.Remove(tmp)
		return 0, ErrSHAMismatch{Want: expectedSHA, Got: got}
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("rename chunk: %w", err)
	}

	// Bump the uploaded_chunks high-water mark. Errors here are not fatal:
	// the chunk is on disk, finalize will succeed regardless of this counter.
	if n > sess.UploadedChunks {
		if err := s.BumpUploadedChunks(ctx, sessionID, n); err != nil {
			s.logger.Warn("bump uploaded_chunks failed", "session", sessionID, "n", n, "err", err)
		}
	}
	return written, nil
}

// ErrSHAMismatch is returned by WriteChunk and Finalize when the computed
// sha256 does not match the expected value.
type ErrSHAMismatch struct {
	Want string
	Got  string
}

func (e ErrSHAMismatch) Error() string {
	return fmt.Sprintf("images: sha256 mismatch: want %s, got %s", e.Want, e.Got)
}

// FinalizeUploadResult is returned by Finalize.
type FinalizeUploadResult struct {
	Image    *Image
	FinalSHA string // computed; same as session.ExpectedSHA256 on success
}

// FinalizeUpload concatenates all chunks in order into the final
// content-addressed file, verifies the running sha256 against the session's
// expected value, optionally consults a metadataExtractor to populate format /
// virtual-size, inserts the images row in the DB, and removes the temp dir.
//
// extractor may be nil; in that case the image is recorded with format
// inferred from the session name (extension) and virtual_size == size_bytes.
//
// If any chunk is missing the call fails before touching the final file.
type metadataExtractor interface {
	Extract(path string) (format string, virtualSize int64, metadataJSON string, err error)
}

func (s *Store) FinalizeUpload(ctx context.Context, sessionID string, extractor metadataExtractor) (*FinalizeUploadResult, error) {
	sess, err := s.GetUpload(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// Verify all chunks present before doing any heavy lifting.
	for i := 1; i <= sess.NumChunks; i++ {
		if _, err := os.Stat(s.chunkPath(sessionID, i)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("images: chunk %d missing", i)
			}
			return nil, fmt.Errorf("stat chunk %d: %w", i, err)
		}
	}

	// Stage the assembled file under .tmp so a partial finalize can't pollute
	// the catalog directory. Rename into place only after the hash check.
	stage := filepath.Join(s.uploadDir(sessionID), "assembled")
	out, err := os.OpenFile(stage, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, finalFileMode)
	if err != nil {
		return nil, fmt.Errorf("create assembled: %w", err)
	}

	h := sha256.New()
	totalBytes, err := s.concatChunks(out, h, sess)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(stage)
		return nil, err
	}
	if totalBytes != sess.TotalSize {
		_ = os.Remove(stage)
		return nil, fmt.Errorf("images: assembled size %d != declared %d", totalBytes, sess.TotalSize)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != sess.ExpectedSHA256 {
		_ = os.Remove(stage)
		return nil, ErrSHAMismatch{Want: sess.ExpectedSHA256, Got: got}
	}

	// Metadata extraction (optional). On error we record the file but log a
	// warning — the catalog should still contain the image so the operator can
	// debug.
	format := inferFormatFromName(sess.Name)
	virtualSize := int64(0)
	metadataJSON := ""
	if extractor != nil {
		f, vs, md, exErr := extractor.Extract(stage)
		if exErr != nil {
			s.logger.Warn("metadata extract failed", "session", sessionID, "err", exErr)
		} else {
			if f != "" {
				format = f
			}
			virtualSize = vs
			metadataJSON = md
		}
	}

	finalPath := s.FinalPath(got, format)
	if err := os.Rename(stage, finalPath); err != nil {
		_ = os.Remove(stage)
		return nil, fmt.Errorf("rename to final: %w", err)
	}

	img, err := s.FinalizeImage(ctx, FinalizeInput{
		Name:         sess.Name,
		Version:      sess.Version,
		Family:       sess.Family,
		Notes:        sess.Notes,
		Format:       format,
		SizeBytes:    totalBytes,
		VirtualSize:  virtualSize,
		SHA256:       got,
		UploadedBy:   sess.UploadedBy,
		MetadataJSON: metadataJSON,
		SessionID:    sess.ID,
	})
	if err != nil {
		// Roll back the file: ErrDuplicate means a parallel finalize won.
		// In all error cases the catalog row was not inserted, so removing
		// the file is the right thing.
		_ = os.Remove(finalPath)
		return nil, err
	}

	// Best-effort cleanup of the chunks dir. Errors here are not fatal.
	if err := os.RemoveAll(s.uploadDir(sessionID)); err != nil {
		s.logger.Warn("cleanup upload dir failed", "session", sessionID, "err", err)
	}

	return &FinalizeUploadResult{Image: img, FinalSHA: got}, nil
}

func (s *Store) concatChunks(w io.Writer, h hash.Hash, sess *UploadSession) (int64, error) {
	var total int64
	mw := io.MultiWriter(w, h)
	for i := 1; i <= sess.NumChunks; i++ {
		path := s.chunkPath(sess.ID, i)
		in, err := os.Open(path)
		if err != nil {
			return total, fmt.Errorf("open chunk %d: %w", i, err)
		}
		n, err := io.Copy(mw, in)
		_ = in.Close()
		if err != nil {
			return total, fmt.Errorf("copy chunk %d: %w", i, err)
		}
		total += n
	}
	return total, nil
}

// AbortUpload removes the session row and its on-disk chunk dir. Returns
// ErrNotFound if the session does not exist.
func (s *Store) AbortUpload(ctx context.Context, sessionID string) error {
	if err := s.DeleteUpload(ctx, sessionID); err != nil {
		return err
	}
	if err := os.RemoveAll(s.uploadDir(sessionID)); err != nil {
		s.logger.Warn("remove upload dir", "session", sessionID, "err", err)
	}
	return nil
}

// DeleteImageFile removes the catalog row and the on-disk file. The file
// removal is best-effort: a missing file is not a failure (the row was the
// source of truth anyway). Returns the deleted image record.
func (s *Store) DeleteImageFile(ctx context.Context, id string) (*Image, error) {
	img, err := s.DeleteImage(ctx, id)
	if err != nil {
		return nil, err
	}
	path := s.FinalPath(img.SHA256, img.Format)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.Warn("remove image file", "path", path, "err", err)
	}
	return img, nil
}

// GCStaleUploads removes session rows older than cutoff and their on-disk
// chunk directories. Intended to be called at startup; ongoing GC can be
// added later if needed.
func (s *Store) GCStaleUploads(ctx context.Context, cutoff time.Time) (int, error) {
	stale, err := s.ListStaleUploads(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	for _, u := range stale {
		if err := s.AbortUpload(ctx, u.ID); err != nil {
			s.logger.Warn("gc stale upload", "session", u.ID, "err", err)
			continue
		}
		s.logger.Info("gc stale upload", "session", u.ID, "name", u.Name, "age", time.Since(u.StartedAt).Round(time.Minute))
	}
	// Also sweep orphan directories under .tmp/ that have no DB row (e.g.,
	// a CreateUpload that crashed before insert, or a long-gone restart).
	tmp := filepath.Join(s.dir, tmpSubdir)
	entries, err := os.ReadDir(tmp)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return len(stale), nil
		}
		return len(stale), fmt.Errorf("read tmp dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || !imageIDRE.MatchString(e.Name()) {
			continue
		}
		if _, err := s.GetUpload(ctx, e.Name()); err == nil {
			continue // live session, leave it alone
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // recent — could be a session about to be inserted
		}
		path := filepath.Join(tmp, e.Name())
		if err := os.RemoveAll(path); err != nil {
			s.logger.Warn("gc orphan dir", "path", path, "err", err)
		} else {
			s.logger.Info("gc orphan dir", "path", path)
		}
	}
	return len(stale), nil
}

// inferFormatFromName picks a format from a filename suffix as a fallback when
// qemu-img is unavailable. Defaults to "qcow2".
func inferFormatFromName(name string) string {
	ext := filepath.Ext(name)
	switch ext {
	case ".raw", ".img":
		return "raw"
	case ".qcow2":
		return "qcow2"
	default:
		return "qcow2"
	}
}
