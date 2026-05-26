// Package bmc stores BMC (IPMI) credentials per machine. The password is
// encrypted at rest with the controller's master key (internal/crypto).
//
// Two read paths exist by design:
//
//   - Get(ctx, uuid)              — returns metadata only (no password). Used
//                                   by the HTTP GET handler so passwords never
//                                   leave the controller process.
//   - GetWithPassword(ctx, uuid)  — returns metadata + decrypted password.
//                                   Used in-process by the future ipmitool
//                                   wrapper (M2.3-4).
//
// Bindings does its own machine_uuid existence check before referencing this
// table; we mirror that pattern (raw SQL against machines) so a delete of
// the machine row also cascades the credential row away.
package bmc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"metalkit/internal/crypto"
)

// Errors callers may need to discriminate on.
var (
	ErrNotFound       = errors.New("bmc: not found")
	ErrMachineUnknown = errors.New("bmc: machine_uuid not in inventory")
)

// Store reads and writes bmc_credentials rows.
type Store struct {
	db     *sql.DB
	logger *slog.Logger
	cipher *crypto.Cipher
}

// NewStore applies the schema. The schema must run *after* the inventory
// schema so the FK reference to machines(uuid) resolves.
func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger, c *crypto.Cipher) (*Store, error) {
	if db == nil {
		return nil, errors.New("bmc: db is required")
	}
	if logger == nil {
		return nil, errors.New("bmc: logger is required")
	}
	if c == nil {
		return nil, errors.New("bmc: cipher is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply bmc schema: %w", err)
	}
	for _, stmt := range migrations {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				return nil, fmt.Errorf("apply bmc migration %q: %w", stmt, err)
			}
		}
	}
	return &Store{db: db, logger: logger, cipher: c}, nil
}

// Credential is the JSON-friendly metadata record. Password is never on the
// wire — see PasswordedCredential for the in-process version.
type Credential struct {
	MachineUUID   string    `json:"machine_uuid"`
	Name          string    `json:"name,omitempty"`
	IP            string    `json:"ip"`
	Port          int       `json:"port"`
	Username      string    `json:"username"`
	IPMIInterface string    `json:"ipmi_interface"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	UpdatedBy     string    `json:"updated_by"`
}

// PasswordedCredential is what the in-process ipmitool wrapper consumes.
// Never marshal this to JSON — there's no `json:` tag on Password to make
// accidental marshaling obvious if someone tries.
type PasswordedCredential struct {
	Credential
	Password string
}

// UpsertInput is what the PUT handler accepts.
//
// Password handling:
//   - On CREATE (no existing row) Password is required and must be non-empty.
//   - On UPDATE (existing row) Password is OPTIONAL: a nil pointer means
//     "keep the current ciphertext on disk untouched"; a non-nil pointer with
//     a non-empty string replaces the password. The empty-string variant is
//     treated as nil (operators omit the field from the JSON body to mean
//     "no change", and the JSON null/missing/"" cases all collapse to "keep").
//
// The change from `string` to `*string` lets the JSON decoder distinguish
// "field absent" from "field present, empty" — the former is the UI's signal
// for "don't change the password".
type UpsertInput struct {
	MachineUUID   string  `json:"-"` // from URL
	Name          string  `json:"name,omitempty"`
	IP            string  `json:"ip"`
	Port          int     `json:"port,omitempty"`
	Username      string  `json:"username"`
	Password      *string `json:"password,omitempty"`
	IPMIInterface string  `json:"ipmi_interface,omitempty"`
	UpdatedBy     string  `json:"-"` // injected by handler
}

// Upsert validates the input, checks the machine exists, and inserts or
// replaces the credential row. Returns ErrMachineUnknown if the machine is
// not in the inventory.
//
// Password semantics: see UpsertInput. On CREATE a non-empty password is
// required; on UPDATE the password may be omitted (nil) or empty to preserve
// the existing ciphertext.
func (s *Store) Upsert(ctx context.Context, in UpsertInput) (*Credential, error) {
	muuid, err := validateMachineUUIDOrPlaceholder(in.MachineUUID)
	if err != nil {
		return nil, err
	}
	ip, err := validateIP(in.IP)
	if err != nil {
		return nil, err
	}
	port, err := validatePort(in.Port)
	if err != nil {
		return nil, err
	}
	user, err := validateUsername(in.Username)
	if err != nil {
		return nil, err
	}
	iface, err := validateInterface(in.IPMIInterface)
	if err != nil {
		return nil, err
	}
	name, err := validateName(in.Name)
	if err != nil {
		return nil, err
	}
	if in.UpdatedBy == "" {
		return nil, errors.New("bmc: updated_by is required")
	}

	// Reject placeholder/IP mismatch: the placeholder UUID is derived from the
	// IP, so they must agree. Otherwise operators could ship an inconsistent
	// pair (placeholder-1.2.3.4 with ip=5.6.7.8) and reconciliation would later
	// match on the wrong row.
	if IsPlaceholderUUID(muuid) {
		expected := PlaceholderUUID(ip)
		if expected != muuid {
			return nil, fmt.Errorf("machine_uuid %q: placeholder must encode ip %q (expected %q)", muuid, ip, expected)
		}
	}

	if err := s.ensureMachineExists(ctx, muuid); err != nil {
		return nil, err
	}

	// "keep the existing password" means: caller omitted the field (nil
	// pointer) OR passed an empty string. Either way we don't re-encrypt.
	keepExisting := in.Password == nil || *in.Password == ""

	exists, err := s.rowExists(ctx, muuid)
	if err != nil {
		return nil, err
	}

	if !exists && keepExisting {
		// New row but no password provided — there's no existing ciphertext
		// to preserve, so this is a hard error (validatePassword's empty
		// branch would return the same message but we want the UI to see
		// the same exact wording it gets today).
		return nil, fmt.Errorf("password: required")
	}

	now := time.Now().Unix()
	nameArg := nullableString(name)
	if keepExisting {
		// UPDATE everything but password_ct.
		_, err = s.db.ExecContext(ctx, `
            UPDATE bmc_credentials SET
                ip             = ?,
                port           = ?,
                username       = ?,
                ipmi_interface = ?,
                name           = ?,
                updated_at     = ?,
                updated_by     = ?
            WHERE machine_uuid = ?`,
			ip, port, user, iface, nameArg, now, in.UpdatedBy, muuid,
		)
		if err != nil {
			return nil, fmt.Errorf("update bmc (no password): %w", err)
		}
		return s.Get(ctx, muuid)
	}

	pass, err := validatePassword(*in.Password)
	if err != nil {
		return nil, err
	}
	ct, err := s.cipher.Encrypt([]byte(pass))
	if err != nil {
		return nil, fmt.Errorf("encrypt password: %w", err)
	}

	// ON CONFLICT keeps created_at from the existing row; only updated_at moves.
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO bmc_credentials
            (machine_uuid, ip, port, username, password_ct, ipmi_interface, name,
             created_at, updated_at, updated_by)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(machine_uuid) DO UPDATE SET
            ip             = excluded.ip,
            port           = excluded.port,
            username       = excluded.username,
            password_ct    = excluded.password_ct,
            ipmi_interface = excluded.ipmi_interface,
            name           = excluded.name,
            updated_at     = excluded.updated_at,
            updated_by     = excluded.updated_by`,
		muuid, ip, port, user, ct, iface, nameArg, now, now, in.UpdatedBy,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert bmc: %w", err)
	}

	return s.Get(ctx, muuid)
}

// rowExists reports whether a credential row already exists for muuid.
// Used to distinguish CREATE from UPDATE so we can enforce "password
// required" only on creation.
func (s *Store) rowExists(ctx context.Context, muuid string) (bool, error) {
	var seen int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM bmc_credentials WHERE machine_uuid = ?`, muuid).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check bmc row: %w", err)
	}
	return true, nil
}

// Get returns the metadata for one credential row. Password is NOT included.
func (s *Store) Get(ctx context.Context, machineUUID string) (*Credential, error) {
	muuid, err := validateMachineUUIDOrPlaceholder(machineUUID)
	if err != nil {
		return nil, err
	}
	var c Credential
	var createdAt, updatedAt int64
	var name sql.NullString
	err = s.db.QueryRowContext(ctx, `
        SELECT machine_uuid, ip, port, username, ipmi_interface, name,
               created_at, updated_at, updated_by
        FROM bmc_credentials WHERE machine_uuid = ?`, muuid).Scan(
		&c.MachineUUID, &c.IP, &c.Port, &c.Username, &c.IPMIInterface, &name,
		&createdAt, &updatedAt, &c.UpdatedBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get bmc: %w", err)
	}
	c.Name = name.String
	c.CreatedAt = time.Unix(createdAt, 0).UTC()
	c.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return &c, nil
}

// GetWithPassword returns the full record including the decrypted password.
// In-process callers only — never expose this over HTTP.
func (s *Store) GetWithPassword(ctx context.Context, machineUUID string) (*PasswordedCredential, error) {
	muuid, err := validateMachineUUIDOrPlaceholder(machineUUID)
	if err != nil {
		return nil, err
	}
	var c Credential
	var createdAt, updatedAt int64
	var name sql.NullString
	var ct []byte
	err = s.db.QueryRowContext(ctx, `
        SELECT machine_uuid, ip, port, username, password_ct, ipmi_interface, name,
               created_at, updated_at, updated_by
        FROM bmc_credentials WHERE machine_uuid = ?`, muuid).Scan(
		&c.MachineUUID, &c.IP, &c.Port, &c.Username, &ct, &c.IPMIInterface, &name,
		&createdAt, &updatedAt, &c.UpdatedBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get bmc: %w", err)
	}
	c.Name = name.String
	c.CreatedAt = time.Unix(createdAt, 0).UTC()
	c.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	pt, err := s.cipher.Decrypt(ct)
	if err != nil {
		return nil, fmt.Errorf("decrypt password for %s: %w", muuid, err)
	}
	return &PasswordedCredential{Credential: c, Password: string(pt)}, nil
}

// List returns metadata for every credential row, sorted by name then machine_uuid
// (named rows surface first, alphabetically, then unnamed rows by UUID).
// Passwords are NOT included.
func (s *Store) List(ctx context.Context) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT machine_uuid, ip, port, username, ipmi_interface, name,
               created_at, updated_at, updated_by
        FROM bmc_credentials
        ORDER BY CASE WHEN name IS NULL OR name = '' THEN 1 ELSE 0 END,
                 name COLLATE NOCASE, machine_uuid`)
	if err != nil {
		return nil, fmt.Errorf("list bmc: %w", err)
	}
	defer rows.Close()
	out := make([]Credential, 0)
	for rows.Next() {
		var c Credential
		var createdAt, updatedAt int64
		var name sql.NullString
		if err := rows.Scan(&c.MachineUUID, &c.IP, &c.Port, &c.Username, &c.IPMIInterface, &name,
			&createdAt, &updatedAt, &c.UpdatedBy); err != nil {
			return nil, fmt.Errorf("scan bmc: %w", err)
		}
		c.Name = name.String
		c.CreatedAt = time.Unix(createdAt, 0).UTC()
		c.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// Delete removes the credential. ErrNotFound if it didn't exist. When the
// machine is a placeholder (BMC-only stub created via PlaceholderUUID), the
// machine row is also deleted — placeholders have no independent existence
// beyond their BMC entry, and a stray placeholder row would never be reconciled.
func (s *Store) Delete(ctx context.Context, machineUUID string) error {
	muuid, err := validateMachineUUIDOrPlaceholder(machineUUID)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM bmc_credentials WHERE machine_uuid = ?`, muuid)
	if err != nil {
		return fmt.Errorf("delete bmc: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if IsPlaceholderUUID(muuid) {
		// Best-effort cleanup of the stub machine row. We don't fail the call
		// if the machine row is already gone (FK cascades from elsewhere can
		// have wiped it) or if some other row still references it.
		if _, err := s.db.ExecContext(ctx, `DELETE FROM machines WHERE uuid = ?`, muuid); err != nil {
			s.logger.Warn("bmc delete: placeholder machine row cleanup failed",
				"uuid", muuid, "err", err)
		}
	}
	return nil
}

// ensureMachineExists is the cross-table referential probe with a twist: when
// the uuid is a placeholder (BMC registered before the machine PXE'd), we
// auto-insert a stub machines row instead of returning ErrMachineUnknown. The
// stub has status='unknown' and blank identity columns, and is later replaced
// when inventory.UpsertReport reconciles by BMC IP.
//
// For real SMBIOS UUIDs the original semantics are preserved: ErrMachineUnknown
// if no machine row exists.
func (s *Store) ensureMachineExists(ctx context.Context, uuid string) error {
	var seen int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM machines WHERE uuid = ?`, uuid).Scan(&seen)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check machine: %w", err)
	}
	// Not found.
	if !IsPlaceholderUUID(uuid) {
		return fmt.Errorf("%w: %s", ErrMachineUnknown, uuid)
	}
	// Auto-create the placeholder machine row. status='unknown' is the only
	// CHECK-permitted starting state for a machine we haven't heard from yet.
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `
        INSERT INTO machines (uuid, first_seen, last_seen, status, latest_report)
        VALUES (?, ?, ?, 'unknown', NULL)`, uuid, now, now); err != nil {
		return fmt.Errorf("create placeholder machine %s: %w", uuid, err)
	}
	s.logger.Info("bmc: created placeholder machine row", "uuid", uuid)
	return nil
}

// FindByIP returns the machine_uuid of the existing bmc_credentials row whose
// IP matches the given address, or "" if none. Used by the POST /api/v1/bmc
// handler to surface a clear 409 instead of silently overwriting.
//
// IP is normalized through validateIP first; an invalid IP returns ErrNotFound.
func (s *Store) FindByIP(ctx context.Context, ip string) (string, error) {
	norm, err := validateIP(ip)
	if err != nil {
		return "", ErrNotFound
	}
	var uuid string
	err = s.db.QueryRowContext(ctx,
		`SELECT machine_uuid FROM bmc_credentials WHERE ip = ? LIMIT 1`, norm).Scan(&uuid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("find bmc by ip: %w", err)
	}
	return uuid, nil
}

// ReconcilePlaceholder is called from inventory.UpsertReport when an agent
// reports a SMBIOS UUID with a BMC IP matching a placeholder credential. It
// migrates the bmc_credentials row to the real UUID and deletes the placeholder
// machine row, all in a single tx so a partial run never leaves orphans.
//
// Returns ErrNotFound if no placeholder credential matches that IP. Returns
// nil if there's nothing to do (no matching placeholder, or the matching row
// already belongs to the real UUID).
func (s *Store) ReconcilePlaceholder(ctx context.Context, realUUID, bmcIP string) error {
	realUUID = strings.ToLower(strings.TrimSpace(realUUID))
	if !smbiosUUIDRE.MatchString(realUUID) {
		return fmt.Errorf("reconcile: real uuid %q is not a SMBIOS UUID", realUUID)
	}
	ip, err := validateIP(bmcIP)
	if err != nil {
		return err
	}
	placeholder := PlaceholderUUID(ip)
	if placeholder == "" {
		return fmt.Errorf("reconcile: cannot derive placeholder uuid from %q", bmcIP)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reconcile: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Does the placeholder credential exist?
	var existingUUID string
	err = tx.QueryRowContext(ctx,
		`SELECT machine_uuid FROM bmc_credentials WHERE machine_uuid = ?`, placeholder).Scan(&existingUUID)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing to reconcile.
		return nil
	}
	if err != nil {
		return fmt.Errorf("reconcile: lookup placeholder: %w", err)
	}

	// Migrate ownership. UPDATE on PRIMARY KEY is allowed in SQLite.
	if _, err := tx.ExecContext(ctx,
		`UPDATE bmc_credentials SET machine_uuid = ? WHERE machine_uuid = ?`,
		realUUID, placeholder); err != nil {
		return fmt.Errorf("reconcile: migrate credential: %w", err)
	}

	// Drop the placeholder machine row. FK cascade on bmc_credentials no
	// longer matches (we just migrated it), so this is a clean delete.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM machines WHERE uuid = ?`, placeholder); err != nil {
		return fmt.Errorf("reconcile: delete placeholder machine: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reconcile: commit: %w", err)
	}
	s.logger.Info("bmc: reconciled placeholder",
		"placeholder", placeholder, "real_uuid", realUUID, "bmc_ip", ip)
	return nil
}

// nullableString returns sql.NullString — empty input becomes NULL on disk so
// the alias column distinguishes "no name set" from a literal empty string.
func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
