package images

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metalkit/internal/sqlitedb"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{Path: path, Logger: logger})
	if err != nil {
		t.Fatalf("sqlitedb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	dir := t.TempDir()
	s, err := NewStore(context.Background(), db, logger, dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// strSHA returns a deterministic 64-hex string from s, padded/truncated as
// needed. Lets tests build distinct, validly-formed sha values cheaply.
func strSHA(s string) string {
	var b strings.Builder
	for b.Len() < 64 {
		for _, c := range s {
			if c >= 'a' && c <= 'f' {
				b.WriteRune(c)
			} else if c >= '0' && c <= '9' {
				b.WriteRune(c)
			} else {
				b.WriteRune(rune('a' + (int(c) % 6)))
			}
			if b.Len() == 64 {
				break
			}
		}
	}
	return b.String()
}

func TestCreateUploadValidates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   CreateUploadInput
		want string
	}{
		{"empty name", CreateUploadInput{TotalSize: 10, ExpectedSHA256: strSHA("a"), UploadedBy: "admin"}, "name is required"},
		{"zero size", CreateUploadInput{Name: "x", ExpectedSHA256: strSHA("a"), UploadedBy: "admin"}, "total_size"},
		{"too big", CreateUploadInput{Name: "x", TotalSize: MaxImageSize + 1, ExpectedSHA256: strSHA("a"), UploadedBy: "admin"}, "exceeds limit"},
		{"bad sha", CreateUploadInput{Name: "x", TotalSize: 10, ExpectedSHA256: "not-hex", UploadedBy: "admin"}, "expected_sha256"},
		{"big chunk", CreateUploadInput{Name: "x", TotalSize: 10, ChunkSize: MaxChunkSize + 1, ExpectedSHA256: strSHA("a"), UploadedBy: "admin"}, "chunk_size"},
		{"no user", CreateUploadInput{Name: "x", TotalSize: 10, ExpectedSHA256: strSHA("a")}, "uploaded_by"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateUpload(ctx, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want contains %q", err, tc.want)
			}
		})
	}
}

func TestCreateUploadOK(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	in := CreateUploadInput{
		Name:           "ubuntu-22.04",
		Family:         "ubuntu",
		Version:        "22.04",
		TotalSize:      25 * 1024 * 1024, // 25 MiB
		ExpectedSHA256: strSHA("abc"),
		UploadedBy:     "admin",
	}
	u, err := s.CreateUpload(ctx, in)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if u.ChunkSize != DefaultChunkSize {
		t.Errorf("ChunkSize: got %d want %d", u.ChunkSize, DefaultChunkSize)
	}
	if u.NumChunks != 3 {
		// 25 MiB / 10 MiB = ceil(2.5) = 3
		t.Errorf("NumChunks: got %d want 3", u.NumChunks)
	}
	if !imageIDRE.MatchString(u.ID) {
		t.Errorf("ID not hex-32: %q", u.ID)
	}

	round, err := s.GetUpload(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUpload: %v", err)
	}
	if round.Name != in.Name || round.Family != in.Family || round.Version != in.Version {
		t.Errorf("round-trip mismatch: %+v", round)
	}
}

func TestCreateUploadDedupes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sha := strSHA("dedupe")
	if _, err := s.FinalizeImage(ctx, FinalizeInput{
		Name: "x", Format: "qcow2", SizeBytes: 1, SHA256: sha, UploadedBy: "admin",
	}); err != nil {
		t.Fatalf("FinalizeImage seed: %v", err)
	}
	_, err := s.CreateUpload(ctx, CreateUploadInput{
		Name: "x", TotalSize: 10, ExpectedSHA256: sha, UploadedBy: "admin",
	})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("CreateUpload: err=%v want ErrDuplicate", err)
	}
}

func TestBumpUploadedChunks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, err := s.CreateUpload(ctx, CreateUploadInput{
		Name: "x", TotalSize: 30 * 1024 * 1024,
		ExpectedSHA256: strSHA("bump"), UploadedBy: "admin",
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	if err := s.BumpUploadedChunks(ctx, u.ID, 2); err != nil {
		t.Fatalf("Bump 2: %v", err)
	}
	round, _ := s.GetUpload(ctx, u.ID)
	if round.UploadedChunks != 2 {
		t.Fatalf("UploadedChunks: got %d want 2", round.UploadedChunks)
	}

	// Lower value must not regress.
	if err := s.BumpUploadedChunks(ctx, u.ID, 1); err != nil {
		t.Fatalf("Bump 1: %v", err)
	}
	round, _ = s.GetUpload(ctx, u.ID)
	if round.UploadedChunks != 2 {
		t.Fatalf("UploadedChunks regressed: got %d", round.UploadedChunks)
	}
}

func TestFinalizeAndList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	u, err := s.CreateUpload(ctx, CreateUploadInput{
		Name: "ubuntu", TotalSize: 100, ExpectedSHA256: strSHA("ub"), UploadedBy: "admin",
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	img, err := s.FinalizeImage(ctx, FinalizeInput{
		Name: "ubuntu", Family: "ubuntu", Format: "qcow2",
		SizeBytes: 100, VirtualSize: 200, SHA256: strSHA("ub"),
		UploadedBy: "admin", SessionID: u.ID, MetadataJSON: `{"foo":"bar"}`,
	})
	if err != nil {
		t.Fatalf("FinalizeImage: %v", err)
	}
	if !imageIDRE.MatchString(img.ID) {
		t.Errorf("image ID not hex-32: %q", img.ID)
	}

	// Session row should be gone.
	if _, err := s.GetUpload(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session row still present after finalize: err=%v", err)
	}

	imgs, err := s.ListImages(ctx)
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(imgs) != 1 || imgs[0].ID != img.ID || imgs[0].Family != "ubuntu" {
		t.Fatalf("ListImages: %+v", imgs)
	}

	got, err := s.GetImage(ctx, img.ID)
	if err != nil || got.SHA256 != img.SHA256 || got.VirtualSize != 200 {
		t.Fatalf("GetImage: %+v err=%v", got, err)
	}
}

func TestFinalizeImageDuplicate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sha := strSHA("dup")
	if _, err := s.FinalizeImage(ctx, FinalizeInput{
		Name: "x", Format: "qcow2", SizeBytes: 1, SHA256: sha, UploadedBy: "admin",
	}); err != nil {
		t.Fatalf("first FinalizeImage: %v", err)
	}
	_, err := s.FinalizeImage(ctx, FinalizeInput{
		Name: "y", Format: "qcow2", SizeBytes: 1, SHA256: sha, UploadedBy: "admin",
	})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second FinalizeImage: err=%v want ErrDuplicate", err)
	}
}

func TestDeleteImage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	img, err := s.FinalizeImage(ctx, FinalizeInput{
		Name: "x", Format: "qcow2", SizeBytes: 1, SHA256: strSHA("del"), UploadedBy: "admin",
	})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	deleted, err := s.DeleteImage(ctx, img.ID)
	if err != nil || deleted.ID != img.ID {
		t.Fatalf("DeleteImage: %+v err=%v", deleted, err)
	}
	if _, err := s.GetImage(ctx, img.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetImage after delete: err=%v", err)
	}
	if _, err := s.DeleteImage(ctx, img.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing: err=%v", err)
	}
}

func TestListStaleUploads(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u1, err := s.CreateUpload(ctx, CreateUploadInput{
		Name: "old", TotalSize: 10, ExpectedSHA256: strSHA("old"), UploadedBy: "admin",
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	// Backdate u1 by an hour.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE upload_sessions SET started_at = ? WHERE id = ?`,
		time.Now().Add(-1*time.Hour).Unix(), u1.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	_, err = s.CreateUpload(ctx, CreateUploadInput{
		Name: "new", TotalSize: 10, ExpectedSHA256: strSHA("new"), UploadedBy: "admin",
	})
	if err != nil {
		t.Fatalf("CreateUpload 2: %v", err)
	}

	stale, err := s.ListStaleUploads(ctx, time.Now().Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("ListStaleUploads: %v", err)
	}
	if len(stale) != 1 || stale[0].ID != u1.ID {
		t.Fatalf("stale: %+v", stale)
	}
}

func TestMaxInFlightUploads(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Manually insert enough rows to hit the cap.
	for i := 0; i < MaxUploadSessions; i++ {
		_, err := s.CreateUpload(ctx, CreateUploadInput{
			Name: "x", TotalSize: 10,
			ExpectedSHA256: strSHA(string(rune('a' + i%6)) + string(rune('0' + i/6))),
			UploadedBy:     "admin",
		})
		if err != nil {
			t.Fatalf("CreateUpload %d: %v", i, err)
		}
	}
	_, err := s.CreateUpload(ctx, CreateUploadInput{
		Name: "over", TotalSize: 10, ExpectedSHA256: strSHA("over"), UploadedBy: "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "too many in-flight") {
		t.Fatalf("expected cap error, got %v", err)
	}
}
