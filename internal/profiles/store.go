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

	"metalkit/internal/components"
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
	// defaultRootPasswordHash is the $6$ sha512crypt hash of the cluster's
	// default root password (see config.DefaultRootPassword). Filled in for
	// any Create where the operator left RootPasswordHash blank — keeps the
	// "fast path" profile creation flow from forcing operators through the
	// hashing dance for a value that will be the same across most profiles.
	// Empty string is allowed (legacy callers / tests); Create then falls
	// back to strict validation and rejects empty input as before.
	defaultRootPasswordHash string
}

// NewStore applies the profiles schema and returns a Store. defaultRootHash
// (optional) is used to backfill CreateInput.RootPasswordHash when blank.
func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger, defaultRootHash string) (*Store, error) {
	if db == nil {
		return nil, errors.New("profiles: db is required")
	}
	if logger == nil {
		return nil, errors.New("profiles: logger is required")
	}
	if defaultRootHash != "" && !sha512cryptRE.MatchString(defaultRootHash) {
		return nil, fmt.Errorf("profiles: default root password hash is not $6$ sha512crypt")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply profiles schema: %w", err)
	}
	// Run idempotent column-additions for pre-existing DBs. SQLite has no
	// IF NOT EXISTS on ADD COLUMN; we execute each ALTER on its own and
	// tolerate "duplicate column" individually (combining them into one
	// multi-statement Exec would skip later statements when an earlier one
	// errors, leaving newer columns un-added on partially-migrated DBs).
	for _, stmt := range strings.Split(migrationSQL, ";") {
		s := strings.TrimSpace(stmt)
		if s == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, s); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return nil, fmt.Errorf("apply profiles migration: %w", err)
		}
	}
	return &Store{db: db, logger: logger, defaultRootPasswordHash: defaultRootHash}, nil
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
	OSFamily         string        `json:"os_family"`
	// SubnetID optionally references a subnets row that supplies CIDR /
	// gateway / DNS / VLAN defaults for any binding that uses this profile.
	// Empty string = no default subnet. Bindings can still override.
	SubnetID string `json:"subnet_id,omitempty"`
	// NetworkRenderer selects which network config rendering strategy to use.
	// Empty string = "auto" (determined by OS family at install time).
	NetworkRenderer string `json:"network_renderer,omitempty"`
	// Bootloader selects which bootloader installation strategy to use.
	// Empty string = "auto" (determined by OS family at install time).
	Bootloader string `json:"bootloader,omitempty"`
	// ChrootDNS is the list of DNS server IPs written to /etc/resolv.conf
	// in the target rootfs during install (used by chroot'd dnf to resolve
	// distro mirrors for kernel-modules installation, etc.). Empty list
	// means "use installer defaults" (223.5.5.5 + 114.114.114.114).
	ChrootDNS []string `json:"chroot_dns,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy string    `json:"created_by"`
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
	OSFamily         string          `json:"os_family,omitempty"`
	SubnetID         string          `json:"subnet_id,omitempty"`
	NetworkRenderer  string          `json:"network_renderer,omitempty"`
	Bootloader       string          `json:"bootloader,omitempty"`
	// ChrootDNS is a comma-or-whitespace-separated list of DNS server IPs
	// to write into /etc/resolv.conf in the target rootfs during install.
	// Empty = use installer defaults. Validated as IPv4/IPv6 literals.
	ChrootDNS        string          `json:"chroot_dns,omitempty"`
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
	OSFamily         *string         `json:"os_family,omitempty"`
	// SubnetID is three-state via *string: nil = unchanged, "" = clear,
	// non-empty = set.
	SubnetID *string `json:"subnet_id,omitempty"`
	// NetworkRenderer is three-state via *string: nil = unchanged, "" = auto,
	// non-empty = set.
	NetworkRenderer *string `json:"network_renderer,omitempty"`
	// Bootloader is three-state via *string: nil = unchanged, "" = auto,
	// non-empty = set.
	Bootloader *string `json:"bootloader,omitempty"`
	// ChrootDNS is three-state via *string: nil = unchanged, "" = use
	// installer defaults, non-empty = comma-separated DNS IPs.
	ChrootDNS *string `json:"chroot_dns,omitempty"`
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
	if in.RootPasswordHash == "" && s.defaultRootPasswordHash != "" {
		in.RootPasswordHash = s.defaultRootPasswordHash
	}
	if !sha512cryptRE.MatchString(in.RootPasswordHash) {
		return nil, errors.New("root_password_hash: must be $6$... sha512crypt (use `mkpasswd -m sha-512` or equivalent)")
	}
	td, err := validateTargetDisk(in.TargetDisk)
	if err != nil {
		return nil, err
	}
	if err := ValidateOSFamily(in.OSFamily); err != nil {
		return nil, err
	}
	osFamily := CanonicalOSFamily(in.OSFamily)
	subnetID, err := validateSubnetID(in.SubnetID)
	if err != nil {
		return nil, err
	}
	if err := components.ValidateNetworkRenderer(in.NetworkRenderer); err != nil {
		return nil, err
	}
	if err := components.ValidateBootloader(in.Bootloader); err != nil {
		return nil, err
	}
	// validateNetwork needs to know whether a subnet is in play so it can
	// skip strict checks on CIDR/gateway/DNS/VLAN (subnet supplies them).
	nc, err := validateNetwork(in.Network, subnetID != "")
	if err != nil {
		return nil, err
	}
	chrootDNS, err := validateChrootDNS(in.ChrootDNS)
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
             target_disk_json, network_json, os_family, subnet_id,
             network_renderer, bootloader, chroot_dns,
             created_at, updated_at, created_by)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Name, in.Description, in.HostnameTemplate, in.RootPasswordHash,
		string(tdBlob), string(ncBlob), osFamily, subnetID,
		in.NetworkRenderer, in.Bootloader, chrootDNS,
		now, now, in.CreatedBy,
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
		OSFamily:         osFamily,
		SubnetID:         subnetID,
		NetworkRenderer:  in.NetworkRenderer,
		Bootloader:       in.Bootloader,
		ChrootDNS:        parseChrootDNS(chrootDNS),
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
		osFamily    sql.NullString
		subnetID    sql.NullString
		networkRenderer sql.NullString
		bootloader  sql.NullString
		chrootDNS   sql.NullString
		createdAt, updatedAt int64
	)
	err := s.db.QueryRowContext(ctx, `
        SELECT id, name, COALESCE(description,''), hostname_template,
               root_password_hash, target_disk_json, network_json,
               COALESCE(os_family,'any'), COALESCE(subnet_id,''),
               COALESCE(network_renderer,''), COALESCE(bootloader,''),
               COALESCE(chroot_dns,''),
               created_at, updated_at, created_by
        FROM profiles WHERE id = ?`, id).Scan(
		&p.ID, &p.Name, &description, &p.HostnameTemplate, &p.RootPasswordHash,
		&tdBlob, &ncBlob, &osFamily, &subnetID, &networkRenderer, &bootloader,
		&chrootDNS,
		&createdAt, &updatedAt, &p.CreatedBy,
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
	p.OSFamily = CanonicalOSFamily(osFamily.String)
	p.SubnetID = subnetID.String
	p.NetworkRenderer = networkRenderer.String
	p.Bootloader = bootloader.String
	p.ChrootDNS = parseChrootDNS(chrootDNS.String)
	p.CreatedAt = time.Unix(createdAt, 0).UTC()
	p.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return &p, nil
}

// List returns every profile, most-recently-created first.
func (s *Store) List(ctx context.Context) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, name, COALESCE(description,''), hostname_template,
               root_password_hash, target_disk_json, network_json,
               COALESCE(os_family,'any'), COALESCE(subnet_id,''),
               COALESCE(network_renderer,''), COALESCE(bootloader,''),
               COALESCE(chroot_dns,''),
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
			osFamily       sql.NullString
			subnetID       sql.NullString
			networkRenderer sql.NullString
			bootloader     sql.NullString
			chrootDNS      sql.NullString
			createdAt, updatedAt int64
		)
		if err := rows.Scan(&p.ID, &p.Name, &description, &p.HostnameTemplate, &p.RootPasswordHash,
			&tdBlob, &ncBlob, &osFamily, &subnetID, &networkRenderer, &bootloader,
			&chrootDNS,
			&createdAt, &updatedAt, &p.CreatedBy); err != nil {
			return nil, fmt.Errorf("scan profile: %w", err)
		}
		p.Description = description.String
		if err := json.Unmarshal([]byte(tdBlob), &p.TargetDisk); err != nil {
			return nil, fmt.Errorf("decode target_disk for %s: %w", p.ID, err)
		}
		if err := json.Unmarshal([]byte(ncBlob), &p.Network); err != nil {
			return nil, fmt.Errorf("decode network for %s: %w", p.ID, err)
		}
		p.OSFamily = CanonicalOSFamily(osFamily.String)
		p.SubnetID = subnetID.String
		p.NetworkRenderer = networkRenderer.String
		p.Bootloader = bootloader.String
		p.ChrootDNS = parseChrootDNS(chrootDNS.String)
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
	// Resolve effective subnet_id BEFORE validating the network blob, since
	// validateNetwork needs to know whether subnet is supplying CIDR/gateway/
	// DNS/VLAN (and should skip strict checks on those fields when so).
	if in.SubnetID != nil {
		sid, err := validateSubnetID(*in.SubnetID)
		if err != nil {
			return nil, err
		}
		cur.SubnetID = sid
	}
	if len(in.Network) > 0 {
		nc, err := validateNetwork(in.Network, cur.SubnetID != "")
		if err != nil {
			return nil, err
		}
		cur.Network = nc
	} else if in.SubnetID != nil && cur.SubnetID != "" {
		// Network not changing this PUT, but subnet just got set/changed.
		// Re-canonicalise the stored network blob so any previously-set
		// static fields / VLAN get stripped (subnet now supplies them).
		cur.Network.PrefixLen = 0
		cur.Network.Gateway = ""
		cur.Network.DNS = nil
		cur.Network.VLAN = 0
	}
	if in.OSFamily != nil {
		if err := ValidateOSFamily(*in.OSFamily); err != nil {
			return nil, err
		}
		cur.OSFamily = CanonicalOSFamily(*in.OSFamily)
	}
	if in.NetworkRenderer != nil {
		v := strings.TrimSpace(*in.NetworkRenderer)
		if err := components.ValidateNetworkRenderer(v); err != nil {
			return nil, err
		}
		cur.NetworkRenderer = v
	}
	if in.Bootloader != nil {
		v := strings.TrimSpace(*in.Bootloader)
		if err := components.ValidateBootloader(v); err != nil {
			return nil, err
		}
		cur.Bootloader = v
	}
	if in.ChrootDNS != nil {
		v, err := validateChrootDNS(*in.ChrootDNS)
		if err != nil {
			return nil, err
		}
		cur.ChrootDNS = parseChrootDNS(v)
	}

	tdBlob, _ := json.Marshal(cur.TargetDisk)
	ncBlob, _ := json.Marshal(cur.Network)
	chrootDNSStr := joinChrootDNS(cur.ChrootDNS)
	now := time.Now().Unix()

	_, err = s.db.ExecContext(ctx, `
        UPDATE profiles
        SET description = ?, hostname_template = ?, root_password_hash = ?,
            target_disk_json = ?, network_json = ?, os_family = ?,
            subnet_id = ?, network_renderer = ?, bootloader = ?,
            chroot_dns = ?, updated_at = ?
        WHERE id = ?`,
		cur.Description, cur.HostnameTemplate, cur.RootPasswordHash,
		string(tdBlob), string(ncBlob), cur.OSFamily, cur.SubnetID,
		cur.NetworkRenderer, cur.Bootloader,
		chrootDNSStr, now, id,
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
