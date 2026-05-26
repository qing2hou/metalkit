// Package images is the metalkit image catalog: chunked upload sessions on
// disk + a SQLite-backed registry of finalized, content-addressed image
// files. The store is the source of truth for what images exist and their
// metadata; the on-disk files at {dir}/{sha256}.{format} hold the bytes.
package images

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrNotFound is returned when an image or upload session lookup misses.
var ErrNotFound = errors.New("images: not found")

// ErrDuplicate is returned when an upload finalizes to a sha256 that already
// exists in the catalog. Callers should surface this as 409 Conflict so the
// client can dedupe by skipping the upload.
var ErrDuplicate = errors.New("images: duplicate sha256")

// Limits enforced by the store/API. Exported so handlers and tests share them.
const (
	MaxImageSize      = int64(10 * 1024 * 1024 * 1024) // 10 GiB
	DefaultChunkSize  = int64(10 * 1024 * 1024)        // 10 MiB
	MaxChunkSize      = int64(64 * 1024 * 1024)        // 64 MiB
	MaxUploadSessions = 32                              // running in parallel
)

// Store is the catalog plus upload-session bookkeeping. It is layered onto a
// shared *sql.DB and does not own its lifecycle.
type Store struct {
	db     *sql.DB
	logger *slog.Logger
	dir    string // root directory for image files and .tmp uploads
}

// NewStore applies the images schema and returns a Store reading and writing
// through db. dir is the on-disk root (e.g. /var/lib/metalkit/images);
// the directory and its .tmp/ subdirectory are created on demand.
func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger, dir string) (*Store, error) {
	if logger == nil {
		return nil, errors.New("images: logger is required")
	}
	if db == nil {
		return nil, errors.New("images: db is required")
	}
	if dir == "" {
		return nil, errors.New("images: dir is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply images schema: %w", err)
	}
	s := &Store{db: db, logger: logger, dir: dir}
	if err := s.ensureDirs(); err != nil {
		return nil, err
	}
	return s, nil
}

// Image is the public record of a finalized image.
type Image struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Version      string    `json:"version,omitempty"`
	Family       string    `json:"family,omitempty"`
	Format       string    `json:"format"`
	SizeBytes    int64     `json:"size_bytes"`
	VirtualSize  int64     `json:"virtual_size,omitempty"`
	SHA256       string    `json:"sha256"`
	UploadedAt   time.Time `json:"uploaded_at"`
	UploadedBy   string    `json:"uploaded_by"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	Notes        string    `json:"notes,omitempty"`
	MetadataJSON string    `json:"metadata_json,omitempty"`
}

// UploadSession captures the state of an in-flight chunked upload.
type UploadSession struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Version        string    `json:"version,omitempty"`
	Family         string    `json:"family,omitempty"`
	Notes          string    `json:"notes,omitempty"`
	ExpectedSHA256 string    `json:"expected_sha256"`
	TotalSize      int64     `json:"total_size"`
	ChunkSize      int64     `json:"chunk_size"`
	NumChunks      int       `json:"num_chunks"`
	UploadedChunks int       `json:"uploaded_chunks"`
	UploadedBy     string    `json:"uploaded_by"`
	StartedAt      time.Time `json:"started_at"`
}

// CreateUploadInput is what /uploads init accepts.
type CreateUploadInput struct {
	Name           string
	Version        string
	Family         string
	Notes          string
	ExpectedSHA256 string
	TotalSize      int64
	ChunkSize      int64
	UploadedBy     string
}

// CreateUpload validates inputs and inserts a new upload_sessions row. It does
// not touch the filesystem; chunk writes lazily create .tmp/{id}/.
func (s *Store) CreateUpload(ctx context.Context, in CreateUploadInput) (*UploadSession, error) {
	if in.Name == "" {
		return nil, errors.New("images: name is required")
	}
	if in.TotalSize <= 0 {
		return nil, errors.New("images: total_size must be positive")
	}
	if in.TotalSize > MaxImageSize {
		return nil, fmt.Errorf("images: total_size %d exceeds limit %d", in.TotalSize, MaxImageSize)
	}
	if !sha256RE.MatchString(in.ExpectedSHA256) {
		return nil, errors.New("images: expected_sha256 must be 64 lowercase hex chars")
	}
	chunkSize := in.ChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	if chunkSize > MaxChunkSize {
		return nil, fmt.Errorf("images: chunk_size %d exceeds limit %d", chunkSize, MaxChunkSize)
	}
	if in.UploadedBy == "" {
		return nil, errors.New("images: uploaded_by is required")
	}

	// Reject early if a finalized image with this sha already exists. The
	// finalize path checks again under transaction, but doing it here saves the
	// user an upload they can't commit.
	var existing string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM images WHERE sha256 = ?`, in.ExpectedSHA256).Scan(&existing)
	if err == nil {
		return nil, fmt.Errorf("%w: image %s already exists", ErrDuplicate, existing)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("dedupe check: %w", err)
	}

	// Cap concurrent sessions so a runaway client cannot exhaust disk by
	// initiating but never finalizing.
	var inFlight int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM upload_sessions`).Scan(&inFlight); err != nil {
		return nil, fmt.Errorf("count sessions: %w", err)
	}
	if inFlight >= MaxUploadSessions {
		return nil, fmt.Errorf("images: too many in-flight uploads (%d); cancel one and retry", inFlight)
	}

	id, err := newUploadID()
	if err != nil {
		return nil, err
	}
	numChunks := int((in.TotalSize + chunkSize - 1) / chunkSize)
	started := time.Now().Unix()

	if _, err := s.db.ExecContext(ctx, `
        INSERT INTO upload_sessions
            (id, name, version, family, notes, expected_sha256, total_size,
             chunk_size, num_chunks, uploaded_chunks, uploaded_by, started_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		id, in.Name, in.Version, in.Family, in.Notes, in.ExpectedSHA256,
		in.TotalSize, chunkSize, numChunks, in.UploadedBy, started,
	); err != nil {
		return nil, fmt.Errorf("insert upload session: %w", err)
	}

	return &UploadSession{
		ID:             id,
		Name:           in.Name,
		Version:        in.Version,
		Family:         in.Family,
		Notes:          in.Notes,
		ExpectedSHA256: in.ExpectedSHA256,
		TotalSize:      in.TotalSize,
		ChunkSize:      chunkSize,
		NumChunks:      numChunks,
		UploadedChunks: 0,
		UploadedBy:     in.UploadedBy,
		StartedAt:      time.Unix(started, 0).UTC(),
	}, nil
}

// GetUpload returns the session row, or ErrNotFound.
func (s *Store) GetUpload(ctx context.Context, id string) (*UploadSession, error) {
	var u UploadSession
	var started int64
	var version, family, notes sql.NullString
	err := s.db.QueryRowContext(ctx, `
        SELECT id, name, version, family, notes, expected_sha256, total_size,
               chunk_size, num_chunks, uploaded_chunks, uploaded_by, started_at
        FROM upload_sessions WHERE id = ?`, id).Scan(
		&u.ID, &u.Name, &version, &family, &notes, &u.ExpectedSHA256, &u.TotalSize,
		&u.ChunkSize, &u.NumChunks, &u.UploadedChunks, &u.UploadedBy, &started,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get upload: %w", err)
	}
	u.Version = version.String
	u.Family = family.String
	u.Notes = notes.String
	u.StartedAt = time.Unix(started, 0).UTC()
	return &u, nil
}

// BumpUploadedChunks advances the counter by one if chunkN was not already
// counted; returns the new total. Idempotent on retry: the caller passes the
// 1-based chunk number, and we use a partial update guarded by a check on
// uploaded_chunks (chunks must be uploaded in order for this counter to mean
// "progress so far"). We tolerate out-of-order chunks at the file layer; the
// counter just reflects the high-water mark.
func (s *Store) BumpUploadedChunks(ctx context.Context, sessionID string, completed int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE upload_sessions SET uploaded_chunks = ? WHERE id = ? AND uploaded_chunks < ?`,
		completed, sessionID, completed)
	if err != nil {
		return fmt.Errorf("update uploaded_chunks: %w", err)
	}
	// Zero rows is fine — it just means another concurrent chunk write already
	// advanced past `completed`.
	_, _ = res.RowsAffected()
	return nil
}

// DeleteUpload removes the session row. The on-disk .tmp/{id}/ is cleaned by
// the caller (uploads.go) so this stays a pure DB op.
func (s *Store) DeleteUpload(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM upload_sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete upload: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// FinalizeInput is what InsertImage receives once the on-disk bytes have been
// hashed, qemu-img-introspected, and moved into place. The store does not
// itself touch the filesystem; uploads.go does that and then calls this to
// commit metadata + delete the now-redundant session row in one transaction.
type FinalizeInput struct {
	Name         string
	Version      string
	Family       string
	Notes        string
	Format       string
	SizeBytes    int64
	VirtualSize  int64
	SHA256       string
	UploadedBy   string
	MetadataJSON string
	SessionID    string // session row to delete on success
}

// FinalizeImage inserts the images row and deletes the session row in a single
// transaction. Returns the populated Image. If the sha256 already exists in
// the catalog the transaction rolls back and ErrDuplicate is returned.
func (s *Store) FinalizeImage(ctx context.Context, in FinalizeInput) (*Image, error) {
	if !sha256RE.MatchString(in.SHA256) {
		return nil, errors.New("images: sha256 must be 64 lowercase hex chars")
	}
	id, err := newImageID()
	if err != nil {
		return nil, err
	}
	uploadedAt := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM images WHERE sha256 = ?`, in.SHA256).Scan(&existing)
	if err == nil {
		return nil, fmt.Errorf("%w: image %s already exists", ErrDuplicate, existing)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("dedupe check: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
        INSERT INTO images
            (id, name, version, family, format, size_bytes, virtual_size,
             sha256, uploaded_at, uploaded_by, notes, metadata_json)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Name, in.Version, in.Family, in.Format, in.SizeBytes, in.VirtualSize,
		in.SHA256, uploadedAt, in.UploadedBy, in.Notes, in.MetadataJSON,
	); err != nil {
		return nil, fmt.Errorf("insert image: %w", err)
	}

	if in.SessionID != "" {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM upload_sessions WHERE id = ?`, in.SessionID); err != nil {
			return nil, fmt.Errorf("delete session: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &Image{
		ID:           id,
		Name:         in.Name,
		Version:      in.Version,
		Family:       in.Family,
		Format:       in.Format,
		SizeBytes:    in.SizeBytes,
		VirtualSize:  in.VirtualSize,
		SHA256:       in.SHA256,
		UploadedAt:   time.Unix(uploadedAt, 0).UTC(),
		UploadedBy:   in.UploadedBy,
		Notes:        in.Notes,
		MetadataJSON: in.MetadataJSON,
	}, nil
}

// ListImages returns all images, most-recently-uploaded first.
func (s *Store) ListImages(ctx context.Context) ([]Image, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, name, COALESCE(version,''), COALESCE(family,''), format,
               size_bytes, COALESCE(virtual_size, 0), sha256, uploaded_at,
               uploaded_by, last_used_at, COALESCE(notes,''),
               COALESCE(metadata_json,'')
        FROM images
        ORDER BY uploaded_at DESC, id DESC
    `)
	if err != nil {
		return nil, fmt.Errorf("query images: %w", err)
	}
	defer rows.Close()

	out := make([]Image, 0)
	for rows.Next() {
		var (
			img             Image
			uploadedAt      int64
			lastUsedAt      sql.NullInt64
		)
		if err := rows.Scan(&img.ID, &img.Name, &img.Version, &img.Family, &img.Format,
			&img.SizeBytes, &img.VirtualSize, &img.SHA256, &uploadedAt,
			&img.UploadedBy, &lastUsedAt, &img.Notes, &img.MetadataJSON); err != nil {
			return nil, fmt.Errorf("scan image: %w", err)
		}
		img.UploadedAt = time.Unix(uploadedAt, 0).UTC()
		if lastUsedAt.Valid {
			t := time.Unix(lastUsedAt.Int64, 0).UTC()
			img.LastUsedAt = &t
		}
		out = append(out, img)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate images: %w", err)
	}
	return out, nil
}

// GetImage looks up a single image by id.
func (s *Store) GetImage(ctx context.Context, id string) (*Image, error) {
	var (
		img        Image
		uploadedAt int64
		lastUsedAt sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
        SELECT id, name, COALESCE(version,''), COALESCE(family,''), format,
               size_bytes, COALESCE(virtual_size, 0), sha256, uploaded_at,
               uploaded_by, last_used_at, COALESCE(notes,''),
               COALESCE(metadata_json,'')
        FROM images WHERE id = ?`, id).Scan(
		&img.ID, &img.Name, &img.Version, &img.Family, &img.Format,
		&img.SizeBytes, &img.VirtualSize, &img.SHA256, &uploadedAt,
		&img.UploadedBy, &lastUsedAt, &img.Notes, &img.MetadataJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get image: %w", err)
	}
	img.UploadedAt = time.Unix(uploadedAt, 0).UTC()
	if lastUsedAt.Valid {
		t := time.Unix(lastUsedAt.Int64, 0).UTC()
		img.LastUsedAt = &t
	}
	return &img, nil
}

// DeleteImage removes the catalog row. The on-disk file is removed by the
// caller (uploads.go) after this returns OK.
func (s *Store) DeleteImage(ctx context.Context, id string) (*Image, error) {
	img, err := s.GetImage(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM images WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("delete image: %w", err)
	}
	return img, nil
}

// ListStaleUploads returns sessions whose started_at is older than cutoff.
// Used by startup-time GC to clean up abandoned uploads.
func (s *Store) ListStaleUploads(ctx context.Context, cutoff time.Time) ([]UploadSession, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, name, version, family, notes, expected_sha256, total_size,
               chunk_size, num_chunks, uploaded_chunks, uploaded_by, started_at
        FROM upload_sessions WHERE started_at < ?`, cutoff.Unix())
	if err != nil {
		return nil, fmt.Errorf("list stale uploads: %w", err)
	}
	defer rows.Close()

	out := make([]UploadSession, 0)
	for rows.Next() {
		var u UploadSession
		var started int64
		var version, family, notes sql.NullString
		if err := rows.Scan(&u.ID, &u.Name, &version, &family, &notes,
			&u.ExpectedSHA256, &u.TotalSize, &u.ChunkSize, &u.NumChunks,
			&u.UploadedChunks, &u.UploadedBy, &started); err != nil {
			return nil, fmt.Errorf("scan stale upload: %w", err)
		}
		u.Version = version.String
		u.Family = family.String
		u.Notes = notes.String
		u.StartedAt = time.Unix(started, 0).UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

func newUploadID() (string, error) {
	return randomHex(16) // 32 hex chars; prefixed below
}

func newImageID() (string, error) {
	return randomHex(16)
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
