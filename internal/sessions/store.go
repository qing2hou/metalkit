// Package sessions persists web UI login sessions in SQLite. The HTTP layer
// (Slice B) consults Get on each request and renews via Touch; the login
// handler issues Create; logout / GC use Delete and DeleteExpired.
package sessions

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Errors callers may need to discriminate on.
var (
	ErrNotFound = errors.New("sessions: not found")
	ErrExpired  = errors.New("sessions: expired")
)

// Session is the JSON-friendly record.
type Session struct {
	ID         string    `json:"id"`
	Username   string    `json:"username"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Store reads and writes session rows. Shares the *sql.DB with the rest of
// the controller.
type Store struct {
	db     *sql.DB
	logger *slog.Logger
	clock  func() time.Time // override in tests
}

// maxUsernameLen caps the username column to a sane size — matches the form
// field's maxlength on the login page (Slice C).
const maxUsernameLen = 64

// NewStore applies the schema (idempotent) and returns a Store.
func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger) (*Store, error) {
	if db == nil {
		return nil, errors.New("sessions: db is required")
	}
	if logger == nil {
		return nil, errors.New("sessions: logger is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply sessions schema: %w", err)
	}
	for _, stmt := range migrations {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				return nil, fmt.Errorf("apply sessions migration %q: %w", stmt, err)
			}
		}
	}
	return &Store{db: db, logger: logger, clock: time.Now}, nil
}

// Create generates a fresh session ID, inserts it, and returns the row.
// username must be non-empty (after trimming); ttl must be > 0.
func (s *Store) Create(ctx context.Context, username string, ttl time.Duration) (*Session, error) {
	uname, err := validateUsername(username)
	if err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("sessions: ttl must be > 0, got %s", ttl)
	}

	id, err := newSessionID()
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}

	now := s.clock().UTC()
	expires := now.Add(ttl)

	_, err = s.db.ExecContext(ctx, `
        INSERT INTO sessions (id, username, created_at, last_seen_at, expires_at)
        VALUES (?, ?, ?, ?, ?)`,
		id, uname, now.Unix(), now.Unix(), expires.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}

	return &Session{
		ID:         id,
		Username:   uname,
		CreatedAt:  time.Unix(now.Unix(), 0).UTC(),
		LastSeenAt: time.Unix(now.Unix(), 0).UTC(),
		ExpiresAt:  time.Unix(expires.Unix(), 0).UTC(),
	}, nil
}

// Get loads a session by ID. Returns ErrNotFound if missing or the ID is
// malformed; ErrExpired if past its expires_at.
func (s *Store) Get(ctx context.Context, id string) (*Session, error) {
	if !isValidSessionID(id) {
		return nil, ErrNotFound
	}
	var sess Session
	var createdAt, lastSeenAt, expiresAt int64
	err := s.db.QueryRowContext(ctx, `
        SELECT id, username, created_at, last_seen_at, expires_at
        FROM sessions WHERE id = ?`, id).Scan(
		&sess.ID, &sess.Username, &createdAt, &lastSeenAt, &expiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	sess.CreatedAt = time.Unix(createdAt, 0).UTC()
	sess.LastSeenAt = time.Unix(lastSeenAt, 0).UTC()
	sess.ExpiresAt = time.Unix(expiresAt, 0).UTC()

	now := s.clock().UTC()
	if !sess.ExpiresAt.After(now) {
		return nil, ErrExpired
	}
	return &sess, nil
}

// Touch is the sliding-renewal write: if last_seen_at is older than minInterval
// the row's last_seen_at advances to now and expires_at to now+ttl. Otherwise
// no-op. Returns the resulting expires_at, or zero time if no-op'd.
// Returns ErrNotFound if the session doesn't exist (or the ID is malformed).
func (s *Store) Touch(ctx context.Context, id string, minInterval, ttl time.Duration) (time.Time, error) {
	if !isValidSessionID(id) {
		return time.Time{}, ErrNotFound
	}
	if ttl <= 0 {
		return time.Time{}, fmt.Errorf("sessions: ttl must be > 0, got %s", ttl)
	}
	if minInterval < 0 {
		return time.Time{}, fmt.Errorf("sessions: minInterval must be >= 0, got %s", minInterval)
	}

	var lastSeenAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT last_seen_at FROM sessions WHERE id = ?`, id).Scan(&lastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("touch session lookup: %w", err)
	}

	now := s.clock().UTC()
	if now.Sub(time.Unix(lastSeenAt, 0)) < minInterval {
		return time.Time{}, nil
	}

	expires := now.Add(ttl)
	res, err := s.db.ExecContext(ctx, `
        UPDATE sessions SET last_seen_at = ?, expires_at = ?
        WHERE id = ?`, now.Unix(), expires.Unix(), id)
	if err != nil {
		return time.Time{}, fmt.Errorf("touch session: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Vanishingly rare race with Delete — treat as not-found.
		return time.Time{}, ErrNotFound
	}
	return time.Unix(expires.Unix(), 0).UTC(), nil
}

// Delete removes a session. Idempotent — no error if it doesn't exist.
// A malformed ID is treated as "doesn't exist" and returns nil.
func (s *Store) Delete(ctx context.Context, id string) error {
	if !isValidSessionID(id) {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteExpired removes all rows where expires_at < now. Returns count removed.
func (s *Store) DeleteExpired(ctx context.Context) (int64, error) {
	now := s.clock().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GCLoop runs DeleteExpired immediately, then every `every` until ctx cancels.
// Logs errors but does not propagate. Intended to be `go store.GCLoop(...)`.
func (s *Store) GCLoop(ctx context.Context, every time.Duration) {
	if every <= 0 {
		s.logger.Error("sessions: GCLoop interval must be > 0", "every", every)
		return
	}
	runOnce := func() {
		n, err := s.DeleteExpired(ctx)
		if err != nil {
			s.logger.Error("sessions: gc failed", "err", err)
			return
		}
		if n > 0 {
			s.logger.Info("sessions: gc removed expired rows", "count", n)
		}
	}
	runOnce()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runOnce()
		}
	}
}

// newSessionID returns 32 random bytes hex-encoded (64-char lowercase hex).
func newSessionID() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// isValidSessionID checks that s is exactly 64 lowercase hex chars. Inlined
// helper — avoids pulling in regexp for one trivial check.
func isValidSessionID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// validateUsername trims, rejects empty, caps length, and forbids control
// chars (incl. NUL). Returns the cleaned-up value.
func validateUsername(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", errors.New("sessions: username is required")
	}
	if len(u) > maxUsernameLen {
		return "", fmt.Errorf("sessions: username exceeds %d chars", maxUsernameLen)
	}
	for _, r := range u {
		// Reject ASCII control chars (0x00-0x1F) and DEL (0x7F).
		if r < 0x20 || r == 0x7f {
			return "", errors.New("sessions: username contains control characters")
		}
	}
	return u, nil
}
