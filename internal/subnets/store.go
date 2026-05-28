// Package subnets is the metalkit subnet catalog. A subnet captures the
// network-segment-level attributes (CIDR, gateway, DNS, optional VLAN) that
// many machines share. Per-machine network state (host IP, bond config) lives
// on the binding and references a subnet by id.
//
// Subnets exist as a separate resource so operators can pre-define their
// network topology once and pick a subnet per-machine at install time without
// re-typing gateway/DNS for every install.
package subnets

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
	ErrNotFound      = errors.New("subnets: not found")
	ErrDuplicateName = errors.New("subnets: duplicate name")
)

type Store struct {
	db     *sql.DB
	logger *slog.Logger
	now    func() time.Time
}

func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger) (*Store, error) {
	if db == nil {
		return nil, errors.New("subnets: db is required")
	}
	if logger == nil {
		return nil, errors.New("subnets: logger is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply subnets schema: %w", err)
	}
	return &Store{db: db, logger: logger, now: time.Now}, nil
}

// Subnet is the public, JSON-friendly record. Gateway and DNS entries are
// stored as canonical IPv4 strings; CIDR is the canonical masked form.
type Subnet struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CIDR        string    `json:"cidr"`
	Gateway     string    `json:"gateway"`
	DNS         []string  `json:"dns"`
	VLANID      int       `json:"vlan_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	CreatedBy   string    `json:"created_by,omitempty"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
}

type CreateInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	CIDR        string   `json:"cidr"`
	Gateway     string   `json:"gateway"`
	DNS         []string `json:"dns,omitempty"`
	VLANID      int      `json:"vlan_id,omitempty"`
	CreatedBy   string   `json:"-"`
}

type UpdateInput struct {
	Description *string  `json:"description,omitempty"`
	CIDR        *string  `json:"cidr,omitempty"`
	Gateway     *string  `json:"gateway,omitempty"`
	DNS         []string `json:"dns,omitempty"`
	VLANID      *int     `json:"vlan_id,omitempty"`
	UpdatedBy   string   `json:"-"`
}

func (s *Store) Create(ctx context.Context, in CreateInput) (*Subnet, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	if !subnetNameRE.MatchString(in.Name) {
		return nil, fmt.Errorf("name %q: 1-64 chars, [A-Za-z0-9._-], must start alnum", in.Name)
	}
	if len(in.Description) > MaxDescriptionLen {
		return nil, fmt.Errorf("description length %d exceeds %d", len(in.Description), MaxDescriptionLen)
	}
	cidr, prefix, err := validateCIDR(in.CIDR)
	if err != nil {
		return nil, err
	}
	gw, err := validateGateway(in.Gateway, prefix)
	if err != nil {
		return nil, err
	}
	dns, err := validateDNS(in.DNS)
	if err != nil {
		return nil, err
	}
	if err := validateVLAN(in.VLANID); err != nil {
		return nil, err
	}
	if in.CreatedBy == "" {
		return nil, errors.New("subnets: created_by is required")
	}

	id, err := newSubnetID()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Unix()
	dnsBlob, _ := json.Marshal(dns)
	var vlanArg any
	if in.VLANID != 0 {
		vlanArg = in.VLANID
	}

	_, err = s.db.ExecContext(ctx, `
        INSERT INTO subnets
            (id, name, description, cidr, gateway, dns_json, vlan_id,
             created_at, updated_at, created_by, updated_by)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Name, in.Description, cidr, gw, string(dnsBlob), vlanArg,
		now, now, in.CreatedBy, in.CreatedBy,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateName, in.Name)
		}
		return nil, fmt.Errorf("insert subnet: %w", err)
	}
	return &Subnet{
		ID: id, Name: in.Name, Description: in.Description,
		CIDR: cidr, Gateway: gw, DNS: dns, VLANID: in.VLANID,
		CreatedAt: time.Unix(now, 0).UTC(),
		UpdatedAt: time.Unix(now, 0).UTC(),
		CreatedBy: in.CreatedBy, UpdatedBy: in.CreatedBy,
	}, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Subnet, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !subnetIDRE.MatchString(id) {
		return nil, fmt.Errorf("subnet id %q: invalid", id)
	}
	row := s.db.QueryRowContext(ctx, selectSubnetSQL+` WHERE id = ?`, id)
	return scanSubnet(row)
}

// GetByName is used by the bindings reverse-migration to find or look up a
// subnet by its operator-facing name.
func (s *Store) GetByName(ctx context.Context, name string) (*Subnet, error) {
	row := s.db.QueryRowContext(ctx, selectSubnetSQL+` WHERE name = ?`, strings.TrimSpace(name))
	return scanSubnet(row)
}

func (s *Store) List(ctx context.Context) ([]Subnet, error) {
	rows, err := s.db.QueryContext(ctx, selectSubnetSQL+` ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list subnets: %w", err)
	}
	defer rows.Close()
	out := make([]Subnet, 0)
	for rows.Next() {
		sn, err := scanSubnet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sn)
	}
	return out, rows.Err()
}

func (s *Store) Update(ctx context.Context, id string, in UpdateInput) (*Subnet, error) {
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
	// CIDR and gateway must be validated together since gateway must be inside CIDR.
	newCIDR := cur.CIDR
	if in.CIDR != nil {
		newCIDR = *in.CIDR
	}
	newGW := cur.Gateway
	if in.Gateway != nil {
		newGW = *in.Gateway
	}
	canonCIDR, prefix, err := validateCIDR(newCIDR)
	if err != nil {
		return nil, err
	}
	canonGW, err := validateGateway(newGW, prefix)
	if err != nil {
		return nil, err
	}
	cur.CIDR = canonCIDR
	cur.Gateway = canonGW
	if in.DNS != nil {
		dns, err := validateDNS(in.DNS)
		if err != nil {
			return nil, err
		}
		cur.DNS = dns
	}
	if in.VLANID != nil {
		if err := validateVLAN(*in.VLANID); err != nil {
			return nil, err
		}
		cur.VLANID = *in.VLANID
	}

	now := s.now().UTC().Unix()
	dnsBlob, _ := json.Marshal(cur.DNS)
	var vlanArg any
	if cur.VLANID != 0 {
		vlanArg = cur.VLANID
	}
	updatedBy := in.UpdatedBy
	if updatedBy == "" {
		updatedBy = cur.UpdatedBy
	}
	_, err = s.db.ExecContext(ctx, `
        UPDATE subnets
        SET description = ?, cidr = ?, gateway = ?, dns_json = ?,
            vlan_id = ?, updated_at = ?, updated_by = ?
        WHERE id = ?`,
		cur.Description, cur.CIDR, cur.Gateway, string(dnsBlob),
		vlanArg, now, updatedBy, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update subnet: %w", err)
	}
	cur.UpdatedAt = time.Unix(now, 0).UTC()
	cur.UpdatedBy = updatedBy
	return cur, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	id = strings.ToLower(strings.TrimSpace(id))
	if !subnetIDRE.MatchString(id) {
		return fmt.Errorf("subnet id %q: invalid", id)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM subnets WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete subnet: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- internal helpers ----

const selectSubnetSQL = `SELECT id, name, description, cidr, gateway, dns_json,
       vlan_id, created_at, updated_at, created_by, updated_by FROM subnets`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSubnet(r rowScanner) (*Subnet, error) {
	var sn Subnet
	var dnsBlob string
	var vlan sql.NullInt64
	var createdAt, updatedAt int64
	if err := r.Scan(
		&sn.ID, &sn.Name, &sn.Description, &sn.CIDR, &sn.Gateway, &dnsBlob,
		&vlan, &createdAt, &updatedAt, &sn.CreatedBy, &sn.UpdatedBy,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan subnet: %w", err)
	}
	if err := json.Unmarshal([]byte(dnsBlob), &sn.DNS); err != nil {
		// Tolerate a malformed blob; surface empty list rather than crash the row.
		sn.DNS = nil
	}
	if vlan.Valid {
		sn.VLANID = int(vlan.Int64)
	}
	sn.CreatedAt = time.Unix(createdAt, 0).UTC()
	sn.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return &sn, nil
}

func newSubnetID() (string, error) {
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
