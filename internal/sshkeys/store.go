package sshkeys

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

var (
	ErrNotFound      = errors.New("sshkeys: not found")
	ErrDuplicateName = errors.New("sshkeys: duplicate name")
)

// Store reads and writes the ssh_keys catalog. Shares the *sql.DB with the
// rest of the controller.
type Store struct {
	db     *sql.DB
	logger *slog.Logger
	now    func() time.Time
}

func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger) (*Store, error) {
	if db == nil {
		return nil, errors.New("sshkeys: db is required")
	}
	if logger == nil {
		return nil, errors.New("sshkeys: logger is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply ssh_keys schema: %w", err)
	}
	for _, m := range migrations {
		if _, err := db.ExecContext(ctx, m); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				return nil, fmt.Errorf("ssh_keys migration %q: %w", m, err)
			}
		}
	}
	return &Store{db: db, logger: logger, now: time.Now}, nil
}

// SSHKey is the public, JSON-friendly record. PublicKey is the canonical
// authorized_keys line; Fingerprint matches `ssh-keygen -lf` output so
// operators can cross-check against their own keyrings.
type SSHKey struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	PublicKey   string    `json:"public_key"`
	KeyType     string    `json:"key_type"`
	Fingerprint string    `json:"fingerprint"`
	Comment     string    `json:"comment,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	CreatedBy   string    `json:"created_by,omitempty"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
}

type CreateInput struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
	CreatedBy string `json:"-"`
}

// UpdateInput is three-state per field: nil pointer = keep the current
// value, non-nil = validate and replace. PublicKey replacement re-derives
// key_type/fingerprint/comment from the new line.
type UpdateInput struct {
	Name      *string `json:"name,omitempty"`
	PublicKey *string `json:"public_key,omitempty"`
	UpdatedBy string  `json:"-"`
}

func (s *Store) Create(ctx context.Context, in CreateInput) (*SSHKey, error) {
	name, err := validateName(in.Name)
	if err != nil {
		return nil, err
	}
	parsed, err := ParsePublicKey(in.PublicKey)
	if err != nil {
		return nil, err
	}
	if in.CreatedBy == "" {
		return nil, errors.New("sshkeys: created_by is required")
	}

	id, err := newKeyID()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Unix()
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO ssh_keys
            (id, name, public_key, key_type, fingerprint, comment,
             created_at, updated_at, created_by, updated_by)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, parsed.PublicKey, parsed.KeyType, parsed.Fingerprint, parsed.Comment,
		now, now, in.CreatedBy, in.CreatedBy,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateName, name)
		}
		return nil, fmt.Errorf("insert ssh key: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *Store) Get(ctx context.Context, id string) (*SSHKey, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !sshKeyIDRE.MatchString(id) {
		return nil, fmt.Errorf("ssh key id %q: invalid", id)
	}
	row := s.db.QueryRowContext(ctx, selectKeySQL+` WHERE id = ?`, id)
	return scanKey(row)
}

func (s *Store) List(ctx context.Context) ([]SSHKey, error) {
	rows, err := s.db.QueryContext(ctx, selectKeySQL+` ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list ssh keys: %w", err)
	}
	defer rows.Close()
	out := []SSHKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func (s *Store) Update(ctx context.Context, id string, in UpdateInput) (*SSHKey, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Name != nil {
		name, err := validateName(*in.Name)
		if err != nil {
			return nil, err
		}
		if name != cur.Name {
			var owner string
			err := s.db.QueryRowContext(ctx,
				`SELECT id FROM ssh_keys WHERE name = ?`, name).Scan(&owner)
			switch {
			case err == nil && owner != id:
				return nil, fmt.Errorf("name %q: already used by another ssh key", name)
			case err != nil && !errors.Is(err, sql.ErrNoRows):
				return nil, fmt.Errorf("name check: %w", err)
			}
			cur.Name = name
		}
	}
	next := ParsedKey{
		PublicKey:   cur.PublicKey,
		KeyType:     cur.KeyType,
		Fingerprint: cur.Fingerprint,
		Comment:     cur.Comment,
	}
	if in.PublicKey != nil {
		parsed, err := ParsePublicKey(*in.PublicKey)
		if err != nil {
			return nil, err
		}
		next = parsed
	}
	now := s.now().UTC().Unix()
	updatedBy := in.UpdatedBy
	if updatedBy == "" {
		updatedBy = cur.UpdatedBy
	}
	_, err = s.db.ExecContext(ctx, `
        UPDATE ssh_keys
        SET name = ?, public_key = ?, key_type = ?, fingerprint = ?, comment = ?,
            updated_at = ?, updated_by = ?
        WHERE id = ?`,
		cur.Name, next.PublicKey, next.KeyType, next.Fingerprint, next.Comment,
		now, updatedBy, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update ssh key: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *Store) Delete(ctx context.Context, id string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	if !sshKeyIDRE.MatchString(id) {
		return fmt.Errorf("ssh key id %q: invalid", id)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM ssh_keys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete ssh key: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- internal helpers ----

const selectKeySQL = `SELECT id, name, public_key, key_type, fingerprint,
       comment, created_at, updated_at, created_by, updated_by FROM ssh_keys`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanKey(r rowScanner) (*SSHKey, error) {
	var k SSHKey
	var createdAt, updatedAt int64
	if err := r.Scan(
		&k.ID, &k.Name, &k.PublicKey, &k.KeyType, &k.Fingerprint,
		&k.Comment, &createdAt, &updatedAt, &k.CreatedBy, &k.UpdatedBy,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan ssh key: %w", err)
	}
	k.CreatedAt = time.Unix(createdAt, 0).UTC()
	k.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return &k, nil
}

// MarshalIDs serialises a canonical id list (ValidateIDs output) into the
// JSON blob stored on the binding row. Empty list → nil so the column stays
// NULL (semantically "no keys selected"), not "[]".
func MarshalIDs(ids []string) []byte {
	if len(ids) == 0 {
		return nil
	}
	blob, _ := json.Marshal(ids)
	return blob
}

func newKeyID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
