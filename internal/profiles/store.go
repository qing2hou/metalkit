// Package profiles is the metalkit install-profile catalog. A profile is a
// reusable template that, combined with a per-machine binding (M2.3-2),
// fully describes how to install an OS image onto a target: hostname pattern,
// root password hash, disk selector, and network method/defaults.
//
// Per-machine state (the actual static IP, BMC creds, etc.) lives in other
// tables. A profile is meant to be applied to many machines.
package profiles

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

// ErrNotFound is returned when a profile lookup misses.
var ErrNotFound = errors.New("profiles: not found")

// ErrDuplicateName is returned when create/update would collide on the UNIQUE
// constraint on profiles.name.
var ErrDuplicateName = errors.New("profiles: duplicate name")

// Store is the profiles catalog. Shares a *sql.DB with the rest of the
// controller; does not own its lifecycle.
type Store struct {
	db     *sql.DB
	logger *slog.Logger
}

// NewStore applies the profiles schema and returns a Store.
func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger) (*Store, error) {
	if db == nil {
		return nil, errors.New("profiles: db is required")
	}
	if logger == nil {
		return nil, errors.New("profiles: logger is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply profiles schema: %w", err)
	}
	return &Store{db: db, logger: logger}, nil
}

// Profile is the public, JSON-friendly record. TargetDisk and Network are
// decoded for callers; in the DB they're stored as JSON columns.
type Profile struct {
	ID               string        `json:"id"`
	Name             string        `json:"name"`
	Description      string        `json:"description,omitempty"`
	HostnameTemplate string        `json:"hostname_template"`
	RootPasswordHash string        `json:"root_password_hash"`
	TargetDisk       TargetDisk    `json:"target_disk"`
	Network          NetworkConfig `json:"network"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
	CreatedBy        string        `json:"created_by"`
}

// CreateInput is what the create handler accepts. JSON sub-objects come in
// as RawMessage so we can validate them via validateTargetDisk/Network.
type CreateInput struct {
	Name             string          `json:"name"`
	Description      string          `json:"description,omitempty"`
	HostnameTemplate string          `json:"hostname_template"`
	RootPasswordHash string          `json:"root_password_hash"`
	TargetDisk       json.RawMessage `json:"target_disk"`
	Network          json.RawMessage `json:"network"`
	CreatedBy        string          `json:"-"` // injected by handler
}

// UpdateInput is what the update handler accepts. All fields are optional;
// nil/empty means "leave unchanged". Pointer for primitives so we can tell
// "absent" from "explicit empty string".
type UpdateInput struct {
	Description      *string         `json:"description,omitempty"`
	HostnameTemplate *string         `json:"hostname_template,omitempty"`
	RootPasswordHash *string         `json:"root_password_hash,omitempty"`
	TargetDisk       json.RawMessage `json:"target_disk,omitempty"`
	Network          json.RawMessage `json:"network,omitempty"`
}

// Create validates input and inserts a new profile row.
func (s *Store) Create(ctx context.Context, in CreateInput) (*Profile, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.HostnameTemplate = strings.TrimSpace(in.HostnameTemplate)
	in.RootPasswordHash = strings.TrimSpace(in.RootPasswordHash)
	in.Description = strings.TrimSpace(in.Description)

	if !profileNameRE.MatchString(in.Name) {
		return nil, fmt.Errorf("name %q: 1-64 chars, [A-Za-z0-9._-], must start alnum", in.Name)
	}
	if len(in.Description) > MaxDescriptionLen {
		return nil, fmt.Errorf("description length %d exceeds %d", len(in.Description), MaxDescriptionLen)
	}
	if err := validateHostnameTemplate(in.HostnameTemplate); err != nil {
		return nil, err
	}
	if !sha512cryptRE.MatchString(in.RootPasswordHash) {
		return nil, errors.New("root_password_hash: must be $6$... sha512crypt (use `mkpasswd -m sha-512` or equivalent)")
	}
	td, err := validateTargetDisk(in.TargetDisk)
	if err != nil {
		return nil, err
	}
	nc, err := validateNetwork(in.Network)
	if err != nil {
		return nil, err
	}
	if in.CreatedBy == "" {
		return nil, errors.New("profiles: created_by is required")
	}

	tdBlob, _ := json.Marshal(td)
	ncBlob, _ := json.Marshal(nc)

	id, err := newProfileID()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()

	_, err = s.db.ExecContext(ctx, `
        INSERT INTO profiles
            (id, name, description, hostname_template, root_password_hash,
             target_disk_json, network_json, created_at, updated_at, created_by)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Name, in.Description, in.HostnameTemplate, in.RootPasswordHash,
		string(tdBlob), string(ncBlob), now, now, in.CreatedBy,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateName, in.Name)
		}
		return nil, fmt.Errorf("insert profile: %w", err)
	}

	return &Profile{
		ID:               id,
		Name:             in.Name,
		Description:      in.Description,
		HostnameTemplate: in.HostnameTemplate,
		RootPasswordHash: in.RootPasswordHash,
		TargetDisk:       td,
		Network:          nc,
		CreatedAt:        time.Unix(now, 0).UTC(),
		UpdatedAt:        time.Unix(now, 0).UTC(),
		CreatedBy:        in.CreatedBy,
	}, nil
}

// Get returns the profile with the given id, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (*Profile, error) {
	var (
		p           Profile
		description sql.NullString
		tdBlob, ncBlob string
		createdAt, updatedAt int64
	)
	err := s.db.QueryRowContext(ctx, `
        SELECT id, name, COALESCE(description,''), hostname_template,
               root_password_hash, target_disk_json, network_json,
               created_at, updated_at, created_by
        FROM profiles WHERE id = ?`, id).Scan(
		&p.ID, &p.Name, &description, &p.HostnameTemplate, &p.RootPasswordHash,
		&tdBlob, &ncBlob, &createdAt, &updatedAt, &p.CreatedBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get profile: %w", err)
	}
	p.Description = description.String
	if err := json.Unmarshal([]byte(tdBlob), &p.TargetDisk); err != nil {
		return nil, fmt.Errorf("decode target_disk for %s: %w", id, err)
	}
	if err := json.Unmarshal([]byte(ncBlob), &p.Network); err != nil {
		return nil, fmt.Errorf("decode network for %s: %w", id, err)
	}
	p.CreatedAt = time.Unix(createdAt, 0).UTC()
	p.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return &p, nil
}

// List returns every profile, most-recently-created first.
func (s *Store) List(ctx context.Context) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, name, COALESCE(description,''), hostname_template,
               root_password_hash, target_disk_json, network_json,
               created_at, updated_at, created_by
        FROM profiles
        ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	defer rows.Close()

	out := make([]Profile, 0)
	for rows.Next() {
		var (
			p              Profile
			description    sql.NullString
			tdBlob, ncBlob string
			createdAt, updatedAt int64
		)
		if err := rows.Scan(&p.ID, &p.Name, &description, &p.HostnameTemplate, &p.RootPasswordHash,
			&tdBlob, &ncBlob, &createdAt, &updatedAt, &p.CreatedBy); err != nil {
			return nil, fmt.Errorf("scan profile: %w", err)
		}
		p.Description = description.String
		if err := json.Unmarshal([]byte(tdBlob), &p.TargetDisk); err != nil {
			return nil, fmt.Errorf("decode target_disk for %s: %w", p.ID, err)
		}
		if err := json.Unmarshal([]byte(ncBlob), &p.Network); err != nil {
			return nil, fmt.Errorf("decode network for %s: %w", p.ID, err)
		}
		p.CreatedAt = time.Unix(createdAt, 0).UTC()
		p.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// Update applies a partial update. Fields left nil/empty stay as-is. Returns
// the post-update Profile.
func (s *Store) Update(ctx context.Context, id string, in UpdateInput) (*Profile, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	if in.Description != nil {
		d := strings.TrimSpace(*in.Description)
		if len(d) > MaxDescriptionLen {
			return nil, fmt.Errorf("description length %d exceeds %d", len(d), MaxDescriptionLen)
		}
		cur.Description = d
	}
	if in.HostnameTemplate != nil {
		t := strings.TrimSpace(*in.HostnameTemplate)
		if err := validateHostnameTemplate(t); err != nil {
			return nil, err
		}
		cur.HostnameTemplate = t
	}
	if in.RootPasswordHash != nil {
		h := strings.TrimSpace(*in.RootPasswordHash)
		if !sha512cryptRE.MatchString(h) {
			return nil, errors.New("root_password_hash: must be $6$... sha512crypt")
		}
		cur.RootPasswordHash = h
	}
	if len(in.TargetDisk) > 0 {
		td, err := validateTargetDisk(in.TargetDisk)
		if err != nil {
			return nil, err
		}
		cur.TargetDisk = td
	}
	if len(in.Network) > 0 {
		nc, err := validateNetwork(in.Network)
		if err != nil {
			return nil, err
		}
		cur.Network = nc
	}

	tdBlob, _ := json.Marshal(cur.TargetDisk)
	ncBlob, _ := json.Marshal(cur.Network)
	now := time.Now().Unix()

	_, err = s.db.ExecContext(ctx, `
        UPDATE profiles
        SET description = ?, hostname_template = ?, root_password_hash = ?,
            target_disk_json = ?, network_json = ?, updated_at = ?
        WHERE id = ?`,
		cur.Description, cur.HostnameTemplate, cur.RootPasswordHash,
		string(tdBlob), string(ncBlob), now, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update profile: %w", err)
	}
	cur.UpdatedAt = time.Unix(now, 0).UTC()
	return cur, nil
}

// Delete removes a profile by id. Returns ErrNotFound if the row didn't
// exist. Caller is responsible for checking that no live bindings reference
// the profile — until M2.3-2 lands we don't have that table; the FK in the
// bindings schema will be ON DELETE RESTRICT.
func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM profiles WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func newProfileID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// isUniqueViolation matches modernc.org/sqlite's error message for a UNIQUE
// constraint failure. We pattern-match on the message because the driver
// returns &sqlite3.Error{Code: 2067} but the type isn't exported in a way we
// can use without pulling the driver as a direct dep.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
