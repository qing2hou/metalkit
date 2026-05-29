// Package settings is a tiny key/value store backed by SQLite. It exists so
// the operator UI can edit a small set of runtime knobs (initially: DHCP
// pool config) without rewriting config.yaml on disk.
//
// The store is intentionally schemaless above the (key, value, updated_at,
// updated_by) row layout — typed validation happens in the higher-level
// API handler, not here. Keys are dot-namespaced strings; values are opaque
// text. Callers that want lists or numbers serialise on the way in.
package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrNotFound is returned by Get when no row exists for the requested key.
// Callers usually want to treat this as "fall back to the YAML default"
// rather than a hard error.
var ErrNotFound = errors.New("settings: not found")

type Store struct {
	db     *sql.DB
	logger *slog.Logger
	now    func() time.Time
}

// Value is one settings row as returned by GetAll. updated_by is the Basic
// Auth user who last wrote the row (or "" for legacy rows).
type Value struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger) (*Store, error) {
	if db == nil {
		return nil, errors.New("settings: db is required")
	}
	if logger == nil {
		return nil, errors.New("settings: logger is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply settings schema: %w", err)
	}
	return &Store{db: db, logger: logger, now: time.Now}, nil
}

// Get returns the value for a single key, or ErrNotFound if no row exists.
func (s *Store) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("settings get %q: %w", key, err)
	}
	return v, nil
}

// GetMany returns the values for a set of keys, omitting any that are not
// present. The returned map is keyed by the input key (verbatim). Useful
// for bulk-loading a feature's settings on startup.
func (s *Store) GetMany(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		v, err := s.Get(ctx, k)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// GetAll returns every row in the table, ordered by key. Intended for the
// debug/admin endpoint that lists every override in effect.
func (s *Store) GetAll(ctx context.Context) ([]Value, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value, updated_at, updated_by FROM settings ORDER BY key ASC`)
	if err != nil {
		return nil, fmt.Errorf("settings list: %w", err)
	}
	defer rows.Close()

	var out []Value
	for rows.Next() {
		var v Value
		var ts int64
		if err := rows.Scan(&v.Key, &v.Value, &ts, &v.UpdatedBy); err != nil {
			return nil, fmt.Errorf("settings scan: %w", err)
		}
		v.UpdatedAt = time.Unix(ts, 0).UTC()
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("settings rows: %w", err)
	}
	return out, nil
}

// Set writes (or overwrites) the value for key. updatedBy is recorded
// verbatim — callers pass the Basic Auth user; "anonymous" is fine when
// auth is disabled.
func (s *Store) Set(ctx context.Context, key, value, updatedBy string) error {
	if key == "" {
		return errors.New("settings: key required")
	}
	ts := s.now().UTC().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at, updated_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`,
		key, value, ts, updatedBy)
	if err != nil {
		return fmt.Errorf("settings set %q: %w", key, err)
	}
	return nil
}

// SetMany writes a batch of (key, value) pairs in a single transaction so
// the UI's "save DHCP form" action is atomic: either every field updates
// or none do.
func (s *Store) SetMany(ctx context.Context, kv map[string]string, updatedBy string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("settings setmany begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ts := s.now().UTC().Unix()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO settings (key, value, updated_at, updated_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`)
	if err != nil {
		return fmt.Errorf("settings setmany prepare: %w", err)
	}
	defer stmt.Close()

	for k, v := range kv {
		if k == "" {
			return errors.New("settings: empty key in batch")
		}
		if _, err := stmt.ExecContext(ctx, k, v, ts, updatedBy); err != nil {
			return fmt.Errorf("settings setmany %q: %w", k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("settings setmany commit: %w", err)
	}
	return nil
}
