package images

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// uploadBytes returns a CreateUploadInput sized to fit body with chunkSize.
func uploadBytes(name string, body []byte, chunkSize int64) CreateUploadInput {
	return CreateUploadInput{
		Name:           name,
		TotalSize:      int64(len(body)),
		ChunkSize:      chunkSize,
		ExpectedSHA256: sha256Hex(body),
		UploadedBy:     "admin",
	}
}

// extractor is a stub metadataExtractor: returns fixed format / virtual_size /
// json regardless of file contents.
type extractor struct {
	format       string
	virtualSize  int64
	metadataJSON string
	err          error
}

func (e extractor) Extract(_ string) (string, int64, string, error) {
	return e.format, e.virtualSize, e.metadataJSON, e.err
}

func TestWriteChunkAndFinalize(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 25 KiB payload, 10 KiB chunks -> 3 chunks (10k, 10k, 5k).
	body := bytes.Repeat([]byte{0xAB}, 25*1024)
	in := uploadBytes("ubuntu.qcow2", body, 10*1024)
	sess, err := s.CreateUpload(ctx, in)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	chunks := [][]byte{body[:10*1024], body[10*1024 : 20*1024], body[20*1024:]}
	for i, c := range chunks {
		n, err := s.WriteChunk(ctx, sess.ID, i+1, bytes.NewReader(c), sha256Hex(c))
		if err != nil {
			t.Fatalf("WriteChunk %d: %v", i+1, err)
		}
		if n != int64(len(c)) {
			t.Errorf("WriteChunk %d: wrote %d want %d", i+1, n, len(c))
		}
	}

	got, err := s.GetUpload(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetUpload: %v", err)
	}
	if got.UploadedChunks != 3 {
		t.Errorf("UploadedChunks: got %d want 3", got.UploadedChunks)
	}

	res, err := s.FinalizeUpload(ctx, sess.ID, extractor{format: "qcow2", virtualSize: 99999})
	if err != nil {
		t.Fatalf("FinalizeUpload: %v", err)
	}
	if res.FinalSHA != in.ExpectedSHA256 {
		t.Errorf("FinalSHA: got %q want %q", res.FinalSHA, in.ExpectedSHA256)
	}
	if res.Image.Format != "qcow2" || res.Image.VirtualSize != 99999 {
		t.Errorf("Image meta: %+v", res.Image)
	}

	// File at content-addressed path with correct bytes.
	final := s.FinalPath(in.ExpectedSHA256, "qcow2")
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if !bytes.Equal(data, body) {
		t.Fatalf("final bytes mismatch")
	}

	// Temp dir cleaned up.
	if _, err := os.Stat(s.uploadDir(sess.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("upload dir not removed: %v", err)
	}

	// Session row deleted.
	if _, err := s.GetUpload(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("session row still present: %v", err)
	}
}

func TestWriteChunkSHAMismatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte{0x01}, 100)
	sess, err := s.CreateUpload(ctx, uploadBytes("x.qcow2", body, 100))
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	_, err = s.WriteChunk(ctx, sess.ID, 1, bytes.NewReader(body), sha256Hex([]byte("wrong")))
	if !errors.As(err, &ErrSHAMismatch{}) {
		t.Fatalf("WriteChunk: err=%v want ErrSHAMismatch", err)
	}
	// File should be cleaned up.
	if _, err := os.Stat(s.chunkPath(sess.ID, 1)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("chunk file not cleaned: %v", err)
	}
}

func TestWriteChunkWrongLength(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte{0x01}, 100)
	sess, err := s.CreateUpload(ctx, uploadBytes("x.qcow2", body, 50))
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	// Body too short.
	_, err = s.WriteChunk(ctx, sess.ID, 1, bytes.NewReader(body[:10]), "")
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Fatalf("WriteChunk short: err=%v want length mismatch", err)
	}
	// Body too long (LimitReader will cap at expectedLen+1).
	long := bytes.Repeat([]byte{0x01}, 100)
	_, err = s.WriteChunk(ctx, sess.ID, 1, bytes.NewReader(long), "")
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Fatalf("WriteChunk long: err=%v want length mismatch", err)
	}
}

func TestWriteChunkOutOfRange(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte{0x01}, 100)
	sess, err := s.CreateUpload(ctx, uploadBytes("x.qcow2", body, 50))
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	_, err = s.WriteChunk(ctx, sess.ID, 99, bytes.NewReader(body), "")
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("WriteChunk out of range: err=%v", err)
	}
}

func TestFinalizeMissingChunk(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte{0x01}, 100)
	sess, err := s.CreateUpload(ctx, uploadBytes("x.qcow2", body, 50))
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	// Upload only chunk 1.
	if _, err := s.WriteChunk(ctx, sess.ID, 1, bytes.NewReader(body[:50]), ""); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	if _, err := s.FinalizeUpload(ctx, sess.ID, nil); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("FinalizeUpload missing: err=%v", err)
	}
	// Session and chunk dir still present after a failed finalize, so retry
	// is possible after uploading the missing chunk.
	if _, err := os.Stat(s.uploadDir(sess.ID)); err != nil {
		t.Errorf("upload dir gone after failed finalize: %v", err)
	}
}

func TestFinalizeBadSHA(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte{0x01}, 100)
	// Declare a wrong expected sha so finalize rejects.
	in := CreateUploadInput{
		Name: "x.qcow2", TotalSize: 100, ChunkSize: 100,
		ExpectedSHA256: strSHA("nopenope"),
		UploadedBy:     "admin",
	}
	sess, err := s.CreateUpload(ctx, in)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, err := s.WriteChunk(ctx, sess.ID, 1, bytes.NewReader(body), ""); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	_, err = s.FinalizeUpload(ctx, sess.ID, nil)
	if !errors.As(err, &ErrSHAMismatch{}) {
		t.Fatalf("FinalizeUpload: err=%v want ErrSHAMismatch", err)
	}
}

func TestAbortAndGC(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte{0x42}, 10)
	sess, err := s.CreateUpload(ctx, uploadBytes("x.qcow2", body, 10))
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, err := s.WriteChunk(ctx, sess.ID, 1, bytes.NewReader(body), ""); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	if err := s.AbortUpload(ctx, sess.ID); err != nil {
		t.Fatalf("AbortUpload: %v", err)
	}
	if _, err := os.Stat(s.uploadDir(sess.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("upload dir not removed: %v", err)
	}
	if _, err := s.GetUpload(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("session row still present: %v", err)
	}
}

func TestDeleteImageFile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte{0x77}, 200)
	sess, _ := s.CreateUpload(ctx, uploadBytes("x.qcow2", body, 200))
	_, _ = s.WriteChunk(ctx, sess.ID, 1, bytes.NewReader(body), "")
	res, err := s.FinalizeUpload(ctx, sess.ID, nil)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	final := s.FinalPath(res.Image.SHA256, res.Image.Format)
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("final missing: %v", err)
	}
	if _, err := s.DeleteImageFile(ctx, res.Image.ID); err != nil {
		t.Fatalf("DeleteImageFile: %v", err)
	}
	if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("final file not removed: %v", err)
	}
}

func TestGCStaleUploads(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Live session: should survive.
	live, _ := s.CreateUpload(ctx, uploadBytes("live.qcow2", []byte("12345"), 5))
	_, _ = s.WriteChunk(ctx, live.ID, 1, bytes.NewReader([]byte("12345")), "")

	// Stale session: backdate.
	stale, _ := s.CreateUpload(ctx, uploadBytes("stale.qcow2", []byte("67890"), 5))
	_, _ = s.WriteChunk(ctx, stale.ID, 1, bytes.NewReader([]byte("67890")), "")
	if _, err := s.db.ExecContext(ctx,
		`UPDATE upload_sessions SET started_at = ? WHERE id = ?`,
		time.Now().Add(-2*time.Hour).Unix(), stale.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Orphan dir with no DB row, also backdated.
	orphan := filepath.Join(s.dir, tmpSubdir, "11111111222222223333333344444444")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	n, err := s.GCStaleUploads(ctx, time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("GCStaleUploads: %v", err)
	}
	if n != 1 {
		t.Errorf("removed sessions: got %d want 1", n)
	}
	if _, err := os.Stat(s.uploadDir(stale.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale upload dir still there")
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("orphan dir still there")
	}
	// Live session survives.
	if _, err := s.GetUpload(ctx, live.ID); err != nil {
		t.Errorf("live session removed: %v", err)
	}
}

func TestInferFormatFromName(t *testing.T) {
	cases := map[string]string{
		"x.qcow2":         "qcow2",
		"foo.raw":         "raw",
		"foo.img":         "raw",
		"weird":           "qcow2",
		"path/sub/x.RAW":  "qcow2", // case-sensitive: ".RAW" doesn't match
	}
	for in, want := range cases {
		if got := inferFormatFromName(in); got != want {
			t.Errorf("inferFormatFromName(%q)=%q want %q", in, got, want)
		}
	}
}

// Stream a body through WriteChunk twice for the same chunk — the second write
// must be a no-op (idempotent overwrite). This exercises the rename-on-tmp
// path: a retry replaces the file atomically.
func TestWriteChunkIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte{0x55}, 50)
	sess, _ := s.CreateUpload(ctx, uploadBytes("x.qcow2", body, 50))
	for i := 0; i < 2; i++ {
		if _, err := s.WriteChunk(ctx, sess.ID, 1, bytes.NewReader(body), sha256Hex(body)); err != nil {
			t.Fatalf("WriteChunk %d: %v", i, err)
		}
	}
	if _, err := s.FinalizeUpload(ctx, sess.ID, nil); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
}

// Ensure io.EOF returns clean even if the chunk reader is empty when expected
// to be non-zero (e.g. zero-byte body).
func TestWriteChunkEmptyBody(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte{0xCC}, 10)
	sess, _ := s.CreateUpload(ctx, uploadBytes("x.qcow2", body, 10))
	_, err := s.WriteChunk(ctx, sess.ID, 1, io.LimitReader(bytes.NewReader(nil), 0), "")
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Fatalf("empty body: err=%v", err)
	}
}
