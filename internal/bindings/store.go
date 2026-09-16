// Package bindings is the metalkit per-machine install assignment table.
// Each row pins one machine to a (image, profile) pair plus the per-machine
// extras (static IP, hostname override) and the operator's desired state.
//
// Bindings is a "current state" table. History of past installs lives in the
// jobs table (M2.3-5). Re-binding a machine overwrites the row in place.
//
// The store performs cross-table referential checks in app code (lookups
// against the machines, images, and profiles tables) rather than relying on
// PRAGMA foreign_keys staying on across pooled connections — the schema
// declares the FKs so future tooling can spot them.
package bindings

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"metalkit/internal/crypto"
	"metalkit/internal/profiles"
)

// Errors callers may need to discriminate on.
var (
	ErrNotFound       = errors.New("bindings: not found")
	ErrMachineUnknown = errors.New("bindings: machine_uuid not in inventory")
	ErrImageUnknown   = errors.New("bindings: image_id not in catalog")
	ErrProfileUnknown = errors.New("bindings: profile_id not in catalog")
	ErrSubnetUnknown  = errors.New("bindings: subnet_id not in catalog")
	ErrFamilyMismatch = errors.New("bindings: image family incompatible with profile os_family")
	// ErrArchMismatch is returned when the image's target CPU architecture
	// conflicts with the machine's reported CPU architecture. Machines that
	// never reported a CPU arch (legacy data) skip the check.
	ErrArchMismatch = errors.New("bindings: image arch incompatible with machine cpu arch")
	ErrInUse        = errors.New("bindings: in use") // for RefCount* helpers
)

// Store reads and writes bindings rows. Shares the *sql.DB with the rest of
// the controller.
type Store struct {
	db     *sql.DB
	logger *slog.Logger
	cipher *crypto.Cipher
	prober IPProber // liveness probe used when auto-allocating static IPs
}

// NewStore applies the bindings schema and returns a Store. The schema must
// run *after* the inventory, images, and profiles schemas so the declared
// FKs reference tables that already exist.
//
// The cipher is used to encrypt per-binding root passwords at rest. It is
// optional — when nil, Upsert refuses non-empty RootPassword input and
// GetPassword always returns ErrNotFound. The wired-up controller always
// passes a cipher; the nil case is for tests that don't exercise passwords.
func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger, cipher *crypto.Cipher) (*Store, error) {
	if db == nil {
		return nil, errors.New("bindings: db is required")
	}
	if logger == nil {
		return nil, errors.New("bindings: logger is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply bindings schema: %w", err)
	}
	// Idempotent column adds for existing databases — SQLite errors with
	// "duplicate column" when the column already exists; we swallow that.
	for _, stmt := range migrations {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				return nil, fmt.Errorf("apply bindings migration %q: %w", stmt, err)
			}
		}
	}
	// ARP first (authoritative on the local L2), ICMP layered on top for
	// routed subnets and ARP-isolated segments — see CompositeProber.
	s := &Store{
		db: db, logger: logger, cipher: cipher,
		prober: &CompositeProber{ARP: &ARPProber{}, ICMP: &ICMPProber{}, Logger: logger},
	}
	return s, nil
}

// WithProber swaps the liveness prober used by IP auto-allocation. Returns
// the store for chaining. Tests inject a deterministic fake; production keeps
// the ARP prober installed by NewStore.
func (s *Store) WithProber(p IPProber) *Store {
	if p != nil {
		s.prober = p
	}
	return s
}

// Binding is the JSON-friendly record.
//
// TargetDisk is the per-binding override for the profile's target_disk.
// nil means "use whatever the profile says"; when set, the jobs spec
// endpoint substitutes it into the profile before sending the spec to
// the agent. We store it as a *TargetDisk pointer so the JSON output
// distinguishes "not overridden" (omitted) from "smallest" (explicit).
//
// Bond is the per-binding override for the profile's network.bond. Same
// three-state semantics as TargetDisk — nil means "use profile.network.bond
// as-is"; when set, the spec endpoint substitutes it (and forces the
// profile's nic_selector to auto, matching validateBond's invariant).
type Binding struct {
	MachineUUID   string               `json:"machine_uuid"`
	ImageID       string               `json:"image_id"`
	ProfileID     string               `json:"profile_id"`
	DesiredState  string               `json:"desired_state"`
	StaticAddress string               `json:"static_address,omitempty"`
	Hostname      string               `json:"hostname,omitempty"`
	TargetDisk    *profiles.TargetDisk `json:"target_disk,omitempty"`
	Bond          *profiles.BondConfig `json:"bond,omitempty"`
	HasPassword   bool                 `json:"has_password"`
	// SubnetID (M2.3-12) optionally references a subnets row. When set, the
	// agent renders network config from that subnet (cidr/gateway/dns/vlan)
	// and treats StaticAddress as the host IP inside it.
	SubnetID string `json:"subnet_id,omitempty"`
	// VLANOverride (M2.3-12) overrides the subnet's vlan_id for this single
	// binding when non-zero. 0 means "use subnet.vlan_id as-is".
	VLANOverride int `json:"vlan_override,omitempty"`
	// NICSelectorOverride is the per-binding override for
	// profile.network.nic_selector. When set, the orchestrator substitutes it
	// into the profile copy before producing the agent spec. Accepts the same
	// shapes as profile.network.nic_selector: "auto", "by-mac:..", "by-name:..".
	// Empty / NULL = inherit profile.
	NICSelectorOverride string    `json:"nic_selector_override,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
	UpdatedBy           string    `json:"updated_by"`
}

// UpsertInput is what the PUT handler accepts.
//
// RootPassword is a three-state field:
//
//	nil      → keep existing ciphertext (no change)
//	""       → clear stored password (revert to profile default)
//	non-empty → encrypt and store; sent as plaintext to the agent at install time
//
// TargetDisk is a three-state field:
//
//	nil          → keep whatever override is currently stored
//	len(raw)==0  → "null" / empty object: clear override (use profile default)
//	non-empty    → validate via profiles.ValidateTargetDisk and store
//
// Bond is a three-state field with the same semantics as TargetDisk —
// nil keeps existing, "null"/"{}" clears, anything else is validated via
// profiles.ValidateBondJSON and stored.
type UpsertInput struct {
	MachineUUID   string          `json:"-"` // taken from URL path
	ImageID       string          `json:"image_id"`
	ProfileID     string          `json:"profile_id"`
	DesiredState  string          `json:"desired_state"`
	StaticAddress string          `json:"static_address,omitempty"`
	Hostname      string          `json:"hostname,omitempty"`
	RootPassword  *string         `json:"root_password,omitempty"`
	TargetDisk    json.RawMessage `json:"target_disk,omitempty"`
	Bond          json.RawMessage `json:"bond,omitempty"`
	// SubnetID is three-state via *string:
	//   nil → keep existing (default)
	//   ""  → explicit clear (NULL)
	//   set → validate (32 hex) + cross-check the subnets row exists
	SubnetID *string `json:"subnet_id,omitempty"`
	// VLANOverride is three-state via *int:
	//   nil → keep existing
	//   0   → explicit clear (NULL)
	//   1..4094 → store as override
	VLANOverride *int `json:"vlan_override,omitempty"`
	// NICSelectorOverride is three-state via *string:
	//   nil → keep existing
	//   ""  → explicit clear (NULL = inherit profile)
	//   set → validate same shapes as profile.network.nic_selector and store
	NICSelectorOverride *string `json:"nic_selector_override,omitempty"`
	UpdatedBy           string  `json:"-"` // injected by handler
}

// Upsert validates the input (including cross-table referential checks and
// profile-aware static_address handling) and inserts or replaces the binding.
//
// Returns ErrMachineUnknown / ErrImageUnknown / ErrProfileUnknown if any
// referenced row is missing — callers should surface those as 422 or 400.
func (s *Store) Upsert(ctx context.Context, in UpsertInput) (*Binding, error) {
	muuid, err := validateMachineUUID(in.MachineUUID)
	if err != nil {
		return nil, err
	}
	imageID, err := validateID("image_id", in.ImageID)
	if err != nil {
		return nil, err
	}
	profileID, err := validateID("profile_id", in.ProfileID)
	if err != nil {
		return nil, err
	}
	desired, err := validateDesiredState(in.DesiredState)
	if err != nil {
		return nil, err
	}
	if in.UpdatedBy == "" {
		return nil, errors.New("bindings: updated_by is required")
	}

	if err := s.checkMachineExists(ctx, muuid); err != nil {
		return nil, err
	}
	if err := s.checkImageExists(ctx, imageID); err != nil {
		return nil, err
	}
	networkMethod, err := s.fetchProfileNetworkMethod(ctx, profileID)
	if err != nil {
		return nil, err
	}
	profileFam, err := s.fetchProfileOSFamily(ctx, profileID)
	if err != nil {
		return nil, err
	}
	imageFam, err := s.fetchImageFamily(ctx, imageID)
	if err != nil {
		return nil, err
	}
	if err := familyCompatible(profileFam, imageFam); err != nil {
		return nil, err
	}
	imageArch, err := s.fetchImageArch(ctx, imageID)
	if err != nil {
		return nil, err
	}
	if err := s.archCompatible(ctx, muuid, imageArch); err != nil {
		return nil, err
	}

	addr, err := validateStaticAddress(in.StaticAddress, networkMethod)
	if err != nil {
		return nil, err
	}
	hostname, err := validateHostname(in.Hostname)
	if err != nil {
		return nil, err
	}

	// Three-state subnet_id, vlan_override and nic_selector_override
	// resolution. Defaults: keep whatever the row currently holds. Empty
	// string / 0 = clear (NULL).
	keepExistingSubnet := in.SubnetID == nil
	keepExistingVLAN := in.VLANOverride == nil
	keepExistingNICSel := in.NICSelectorOverride == nil

	var existingSubnet sql.NullString
	var existingVLAN sql.NullInt64
	var existingNICSel sql.NullString
	if keepExistingSubnet || keepExistingVLAN || keepExistingNICSel {
		_ = s.db.QueryRowContext(ctx,
			`SELECT subnet_id, vlan_override, nic_selector_override FROM bindings WHERE machine_uuid = ?`, muuid).
			Scan(&existingSubnet, &existingVLAN, &existingNICSel)
	}

	var subnetIDSQL any
	var resolvedSubnetID string
	if keepExistingSubnet {
		if existingSubnet.Valid && existingSubnet.String != "" {
			subnetIDSQL = existingSubnet.String
			resolvedSubnetID = existingSubnet.String
		}
	} else {
		sid, err := validateSubnetID(*in.SubnetID)
		if err != nil {
			return nil, err
		}
		if sid != "" {
			if err := s.checkSubnetExists(ctx, sid); err != nil {
				return nil, err
			}
			subnetIDSQL = sid
			resolvedSubnetID = sid
		}
	}

	// When a subnet is in effect AND the profile uses static addressing, the
	// host IP (static_address) must lie inside its CIDR and may not collide with
	// the gateway/network/broadcast. If static_address is empty, we auto-allocate
	// an unused IP from the subnet. For DHCP profiles we never assign a static
	// address — the host gets its lease from DHCP at boot.
	if resolvedSubnetID != "" && networkMethod == "static" {
		cidr, gw, err := s.fetchSubnetCIDRGateway(ctx, resolvedSubnetID)
		if err != nil {
			return nil, err
		}

		// Auto-allocate IP if not provided
		if addr == "" {
			allocated, err := s.allocateIPFromSubnet(ctx, muuid, cidr, gw)
			if err != nil {
				return nil, fmt.Errorf("auto-allocate IP: %w", err)
			}
			addr = allocated
			s.logger.Info("auto-allocated IP for binding",
				"machine_uuid", muuid, "subnet_id", resolvedSubnetID, "ip", addr)
		} else {
			// Validate provided IP
			if err := hostInSubnet(addr, cidr, gw); err != nil {
				return nil, fmt.Errorf("static_address %w", err)
			}
		}
	}

	// Final guard: a static binding with no IP and no subnet is unconfigurable —
	// the profile won't know what address to write, and there's no pool to draw
	// from. validateStaticAddress used to error here; we deferred the check so
	// the auto-allocate path above could run when a subnet IS bound.
	if networkMethod == "static" && addr == "" {
		return nil, errors.New("static_address: required when profile.network.method=static and no subnet is bound (bind a subnet to auto-allocate)")
	}

	var vlanSQL any
	if keepExistingVLAN {
		if existingVLAN.Valid && existingVLAN.Int64 != 0 {
			vlanSQL = existingVLAN.Int64
		}
	} else {
		writeNull, val, err := validateVLANOverride(in.VLANOverride)
		if err != nil {
			return nil, err
		}
		if !writeNull {
			vlanSQL = val
		}
	}

	var nicSelectorSQL any
	if keepExistingNICSel {
		if existingNICSel.Valid && existingNICSel.String != "" {
			nicSelectorSQL = existingNICSel.String
		}
	} else {
		sel, err := validateNICSelectorOverride(*in.NICSelectorOverride)
		if err != nil {
			return nil, err
		}
		if sel != "" {
			nicSelectorSQL = sel
		}
	}

	now := time.Now().Unix()
	var addrSQL, hostnameSQL any
	if addr == "" {
		addrSQL = nil
	} else {
		addrSQL = addr
	}
	if hostname == "" {
		hostnameSQL = nil
	} else {
		hostnameSQL = hostname
	}

	// Three-state password handling:
	//   nil       → keep existing ciphertext (load from row, if any)
	//   ""        → explicit clear (NULL)
	//   non-empty → encrypt and store
	// Three-state target_disk handling:
	//   nil raw       → keep existing override (load from row, if any)
	//   "null" / "{}" → explicit clear (NULL → fall back to profile default)
	//   valid JSON    → validate via profiles.ValidateTargetDisk, store
	// Three-state bond handling: same semantics as target_disk.
	// We pre-fetch the existing row when any knob is in "keep" mode so a
	// single INSERT … ON CONFLICT DO UPDATE statement covers every case.
	keepExistingPassword := in.RootPassword == nil
	keepExistingTargetDisk := in.TargetDisk == nil
	keepExistingBond := in.Bond == nil

	var existingPasswordCT []byte
	var existingTargetDisk, existingBond sql.NullString
	if keepExistingPassword || keepExistingTargetDisk || keepExistingBond {
		_ = s.db.QueryRowContext(ctx,
			`SELECT root_password_enc, target_disk_override, bond_override FROM bindings WHERE machine_uuid = ?`, muuid).
			Scan(&existingPasswordCT, &existingTargetDisk, &existingBond)
		// QueryRow err is silently ignored: a missing row just leaves zero
		// values, which is the same as "first insert, nothing to keep".
	}

	var passwordCT []byte
	if keepExistingPassword {
		passwordCT = existingPasswordCT
	} else if *in.RootPassword != "" {
		if s.cipher == nil {
			return nil, errors.New("bindings: cipher is required to set a password")
		}
		if err := validatePassword(*in.RootPassword); err != nil {
			return nil, err
		}
		ct, err := s.cipher.Encrypt([]byte(*in.RootPassword))
		if err != nil {
			return nil, fmt.Errorf("encrypt password: %w", err)
		}
		passwordCT = ct
	}

	var tdSQL any
	if keepExistingTargetDisk {
		if existingTargetDisk.Valid {
			tdSQL = existingTargetDisk.String
		}
	} else if isClearJSON(in.TargetDisk) {
		// explicit clear — leave tdSQL nil for NULL
	} else {
		td, err := profiles.ValidateTargetDisk(in.TargetDisk)
		if err != nil {
			return nil, err
		}
		blob, err := json.Marshal(td)
		if err != nil {
			return nil, fmt.Errorf("marshal target_disk: %w", err)
		}
		tdSQL = string(blob)
	}

	var bondSQL any
	if keepExistingBond {
		if existingBond.Valid {
			bondSQL = existingBond.String
		}
	} else if isClearJSON(in.Bond) {
		// explicit clear — leave bondSQL nil for NULL
	} else {
		bondCfg, err := profiles.ValidateBondJSON(in.Bond)
		if err != nil {
			return nil, err
		}
		blob, err := json.Marshal(bondCfg)
		if err != nil {
			return nil, fmt.Errorf("marshal bond: %w", err)
		}
		bondSQL = string(blob)
	}

	var ctSQL any
	if len(passwordCT) > 0 {
		ctSQL = passwordCT
	}

	_, err = s.db.ExecContext(ctx, `
        INSERT INTO bindings
            (machine_uuid, image_id, profile_id, desired_state,
             static_address, hostname, root_password_enc, target_disk_override,
             bond_override, subnet_id, vlan_override, nic_selector_override,
             updated_at, updated_by)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(machine_uuid) DO UPDATE SET
            image_id              = excluded.image_id,
            profile_id            = excluded.profile_id,
            desired_state         = excluded.desired_state,
            static_address        = excluded.static_address,
            hostname              = excluded.hostname,
            root_password_enc     = excluded.root_password_enc,
            target_disk_override  = excluded.target_disk_override,
            bond_override         = excluded.bond_override,
            subnet_id             = excluded.subnet_id,
            vlan_override         = excluded.vlan_override,
            nic_selector_override = excluded.nic_selector_override,
            updated_at            = excluded.updated_at,
            updated_by            = excluded.updated_by`,
		muuid, imageID, profileID, desired, addrSQL, hostnameSQL, ctSQL, tdSQL, bondSQL, subnetIDSQL, vlanSQL, nicSelectorSQL, now, in.UpdatedBy,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert binding: %w", err)
	}

	return s.Get(ctx, muuid)
}

// Get returns the binding for a machine, or ErrNotFound.
func (s *Store) Get(ctx context.Context, machineUUID string) (*Binding, error) {
	muuid, err := validateMachineUUID(machineUUID)
	if err != nil {
		return nil, err
	}
	var b Binding
	var addr, hostname, tdJSON, bondJSON, subnetID, nicSelOverride sql.NullString
	var vlanOverride sql.NullInt64
	var updatedAt int64
	var passwordCT []byte
	err = s.db.QueryRowContext(ctx, `
        SELECT machine_uuid, image_id, profile_id, desired_state,
               static_address, hostname, root_password_enc, target_disk_override,
               bond_override, subnet_id, vlan_override, nic_selector_override,
               updated_at, updated_by
        FROM bindings WHERE machine_uuid = ?`, muuid).Scan(
		&b.MachineUUID, &b.ImageID, &b.ProfileID, &b.DesiredState,
		&addr, &hostname, &passwordCT, &tdJSON, &bondJSON, &subnetID, &vlanOverride, &nicSelOverride, &updatedAt, &b.UpdatedBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get binding: %w", err)
	}
	b.StaticAddress = addr.String
	b.Hostname = hostname.String
	b.HasPassword = len(passwordCT) > 0
	if tdJSON.Valid && tdJSON.String != "" {
		var td profiles.TargetDisk
		if err := json.Unmarshal([]byte(tdJSON.String), &td); err == nil {
			b.TargetDisk = &td
		}
	}
	if bondJSON.Valid && bondJSON.String != "" {
		var bc profiles.BondConfig
		if err := json.Unmarshal([]byte(bondJSON.String), &bc); err == nil {
			b.Bond = &bc
		}
	}
	if subnetID.Valid {
		b.SubnetID = subnetID.String
	}
	if vlanOverride.Valid {
		b.VLANOverride = int(vlanOverride.Int64)
	}
	if nicSelOverride.Valid {
		b.NICSelectorOverride = nicSelOverride.String
	}
	b.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return &b, nil
}

// List returns every binding. No ordering guarantee beyond stable-ish
// (PRIMARY KEY order, i.e. by SMBIOS UUID).
func (s *Store) List(ctx context.Context) ([]Binding, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT machine_uuid, image_id, profile_id, desired_state,
               static_address, hostname, root_password_enc, target_disk_override,
               bond_override, subnet_id, vlan_override, nic_selector_override,
               updated_at, updated_by
        FROM bindings ORDER BY machine_uuid`)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	defer rows.Close()
	out := make([]Binding, 0)
	for rows.Next() {
		var b Binding
		var addr, hostname, tdJSON, bondJSON, subnetID, nicSelOverride sql.NullString
		var vlanOverride sql.NullInt64
		var updatedAt int64
		var passwordCT []byte
		if err := rows.Scan(&b.MachineUUID, &b.ImageID, &b.ProfileID, &b.DesiredState,
			&addr, &hostname, &passwordCT, &tdJSON, &bondJSON, &subnetID, &vlanOverride, &nicSelOverride, &updatedAt, &b.UpdatedBy); err != nil {
			return nil, fmt.Errorf("scan binding: %w", err)
		}
		b.StaticAddress = addr.String
		b.Hostname = hostname.String
		b.HasPassword = len(passwordCT) > 0
		if tdJSON.Valid && tdJSON.String != "" {
			var td profiles.TargetDisk
			if err := json.Unmarshal([]byte(tdJSON.String), &td); err == nil {
				b.TargetDisk = &td
			}
		}
		if bondJSON.Valid && bondJSON.String != "" {
			var bc profiles.BondConfig
			if err := json.Unmarshal([]byte(bondJSON.String), &bc); err == nil {
				b.Bond = &bc
			}
		}
		if subnetID.Valid {
			b.SubnetID = subnetID.String
		}
		if vlanOverride.Valid {
			b.VLANOverride = int(vlanOverride.Int64)
		}
		if nicSelOverride.Valid {
			b.NICSelectorOverride = nicSelOverride.String
		}
		b.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetPassword decrypts and returns the binding's root password in plaintext.
// Returns ErrNotFound if the binding doesn't exist or has no password set.
// Use sparingly — this is the entire reason the master.key file matters.
func (s *Store) GetPassword(ctx context.Context, machineUUID string) (string, error) {
	muuid, err := validateMachineUUID(machineUUID)
	if err != nil {
		return "", err
	}
	if s.cipher == nil {
		return "", errors.New("bindings: cipher not configured")
	}
	var ct []byte
	err = s.db.QueryRowContext(ctx,
		`SELECT root_password_enc FROM bindings WHERE machine_uuid = ?`, muuid).Scan(&ct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get binding password: %w", err)
	}
	if len(ct) == 0 {
		return "", ErrNotFound
	}
	pt, err := s.cipher.Decrypt(ct)
	if err != nil {
		return "", fmt.Errorf("decrypt password: %w", err)
	}
	return string(pt), nil
}

// Delete removes the binding for a machine. ErrNotFound if it didn't exist.
func (s *Store) Delete(ctx context.Context, machineUUID string) error {
	muuid, err := validateMachineUUID(machineUUID)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM bindings WHERE machine_uuid = ?`, muuid)
	if err != nil {
		return fmt.Errorf("delete binding: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteByImage removes every binding referencing the given image id.
// Used by the images delete handler to cascade-delete bindings that would
// otherwise block image removal via the FOREIGN KEY constraint. Returns the
// number of bindings removed.
func (s *Store) DeleteByImage(ctx context.Context, imageID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bindings WHERE image_id = ?`, imageID)
	if err != nil {
		return 0, fmt.Errorf("delete bindings by image: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RefCountByImage returns how many bindings reference the given image id.
// Callers (images delete handler) should refuse the delete if > 0.
func (s *Store) RefCountByImage(ctx context.Context, imageID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bindings WHERE image_id = ?`, imageID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count bindings for image: %w", err)
	}
	return n, nil
}

// RefCountByProfile is the profile equivalent of RefCountByImage.
func (s *Store) RefCountByProfile(ctx context.Context, profileID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bindings WHERE profile_id = ?`, profileID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count bindings for profile: %w", err)
	}
	return n, nil
}

// DeleteByProfile removes every binding referencing the given profile id.
// Used by the profiles delete handler to cascade-delete bindings that would
// otherwise block profile removal via the FOREIGN KEY constraint.
func (s *Store) DeleteByProfile(ctx context.Context, profileID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bindings WHERE profile_id = ?`, profileID)
	if err != nil {
		return 0, fmt.Errorf("delete bindings by profile: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RefCountBySubnet returns how many bindings reference the given subnet id.
// Callers (subnets delete handler) should refuse the delete if > 0.
func (s *Store) RefCountBySubnet(ctx context.Context, subnetID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bindings WHERE subnet_id = ?`, subnetID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count bindings for subnet: %w", err)
	}
	return n, nil
}

// ClearDesiredState flips desired_state to 'none' for a machine. Called by the
// jobs orchestrator after a successful install + bootdev=disk handshake so
// the next reconciliation tick won't re-create another install job. Idempotent:
// returns nil when the row is already cleared or doesn't exist.
func (s *Store) ClearDesiredState(ctx context.Context, machineUUID, updatedBy string) error {
	muuid, err := validateMachineUUID(machineUUID)
	if err != nil {
		return err
	}
	if updatedBy == "" {
		return errors.New("bindings: updated_by is required")
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
        UPDATE bindings SET desired_state = 'none', updated_at = ?, updated_by = ?
        WHERE machine_uuid = ? AND desired_state != 'none'`,
		now, updatedBy, muuid)
	if err != nil {
		return fmt.Errorf("clear desired_state: %w", err)
	}
	return nil
}

// checkMachineExists / checkImageExists / fetchProfileNetworkMethod are the
// cross-table referential checks. They keep the bindings package decoupled
// from the inventory/images/profiles packages (no Go imports) by issuing
// plain queries against the shared DB schema.

func (s *Store) checkMachineExists(ctx context.Context, uuid string) error {
	var seen int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM machines WHERE uuid = ?`, uuid).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrMachineUnknown, uuid)
	}
	if err != nil {
		return fmt.Errorf("check machine: %w", err)
	}
	return nil
}

// checkSubnetExists confirms the referenced subnet is in the catalog. Returns
// ErrSubnetUnknown otherwise. Mirrors the machine/image checks above.
func (s *Store) checkSubnetExists(ctx context.Context, id string) error {
	var seen int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM subnets WHERE id = ?`, id).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrSubnetUnknown, id)
	}
	if err != nil {
		return fmt.Errorf("check subnet: %w", err)
	}
	return nil
}

// fetchSubnetCIDRGateway reads the CIDR and gateway of a subnet for the
// host-in-subnet check. Returns ErrSubnetUnknown if missing.
func (s *Store) fetchSubnetCIDRGateway(ctx context.Context, id string) (cidr, gateway string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT cidr, gateway FROM subnets WHERE id = ?`, id).Scan(&cidr, &gateway)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("%w: %s", ErrSubnetUnknown, id)
	}
	if err != nil {
		return "", "", fmt.Errorf("fetch subnet cidr/gateway: %w", err)
	}
	return cidr, gateway, nil
}

func (s *Store) checkImageExists(ctx context.Context, id string) error {
	var seen int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM images WHERE id = ?`, id).Scan(&seen)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrImageUnknown, id)
	}
	if err != nil {
		return fmt.Errorf("check image: %w", err)
	}
	return nil
}

// fetchProfileNetworkMethod reads only the field we need to validate the
// static_address. We poke into the JSON blob with json_extract because we'd
// rather not pull profiles into bindings as a Go dependency.
func (s *Store) fetchProfileNetworkMethod(ctx context.Context, id string) (string, error) {
	var method string
	err := s.db.QueryRowContext(ctx,
		`SELECT json_extract(network_json, '$.method') FROM profiles WHERE id = ?`, id).Scan(&method)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrProfileUnknown, id)
	}
	if err != nil {
		return "", fmt.Errorf("fetch profile network method: %w", err)
	}
	method = strings.ToLower(method)
	if method != "static" && method != "dhcp" {
		// Should be impossible: profiles validator rejects other methods.
		return "", fmt.Errorf("profile %s: stored method %q is not static or dhcp", id, method)
	}
	return method, nil
}

// fetchProfileOSFamily returns the profile's declared os_family (canonical
// lowercase; "any" if NULL/empty). ErrProfileUnknown if the row is missing.
func (s *Store) fetchProfileOSFamily(ctx context.Context, id string) (string, error) {
	var fam sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(os_family,'any') FROM profiles WHERE id = ?`, id).Scan(&fam)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrProfileUnknown, id)
	}
	if err != nil {
		return "", fmt.Errorf("fetch profile os_family: %w", err)
	}
	s2 := strings.ToLower(strings.TrimSpace(fam.String))
	if s2 == "" {
		return "any", nil
	}
	return s2, nil
}

// fetchImageFamily returns the image's declared family (lowercase; "" if
// unset). ErrImageUnknown if the row is missing.
func (s *Store) fetchImageFamily(ctx context.Context, id string) (string, error) {
	var fam sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(family,'') FROM images WHERE id = ?`, id).Scan(&fam)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrImageUnknown, id)
	}
	if err != nil {
		return "", fmt.Errorf("fetch image family: %w", err)
	}
	return strings.ToLower(strings.TrimSpace(fam.String)), nil
}

// fetchImageArch returns the image's target arch (lowercase; "" if unset —
// legacy images uploaded before the arch column existed). ErrImageUnknown if
// the row is missing.
func (s *Store) fetchImageArch(ctx context.Context, id string) (string, error) {
	var arch sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(arch,'') FROM images WHERE id = ?`, id).Scan(&arch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrImageUnknown, id)
	}
	if err != nil {
		return "", fmt.Errorf("fetch image arch: %w", err)
	}
	return strings.ToLower(strings.TrimSpace(arch.String)), nil
}

// archCompatible rejects bindings that would flash an image built for one
// CPU architecture onto a machine reporting another. Either side may be
// unknown (""): legacy images carry no arch, and machines that never
// PXE-booted into the live agent have no report — in those cases the check
// passes (soft until data exists on both ends).
func (s *Store) archCompatible(ctx context.Context, muuid, imageArch string) error {
	if imageArch == "" {
		return nil // legacy image, no opinion
	}
	var machineArch sql.NullString
	err := s.db.QueryRowContext(ctx, `
        SELECT json_extract(r.body, '$.cpu.arch')
        FROM machines m
        LEFT JOIN reports r ON r.id = m.latest_report
        WHERE m.uuid = ?`, muuid).Scan(&machineArch)
	if err != nil {
		return fmt.Errorf("fetch machine arch: %w", err)
	}
	mArch := strings.ToLower(strings.TrimSpace(machineArch.String))
	if mArch == "" {
		return nil // machine never reported its arch — can't judge
	}
	// The report uses Go's runtime.GOARCH / uname -m vocabulary: amd64 or
	// arm64 on our agent builds. Normalise the x86_64/aarch64 aliases just
	// in case a future collector changes sources.
	switch mArch {
	case "x86_64", "amd64":
		mArch = "amd64"
	case "aarch64", "arm64":
		mArch = "arm64"
	}
	if mArch != imageArch {
		return fmt.Errorf("%w: image is %q, machine reports %q",
			ErrArchMismatch, imageArch, mArch)
	}
	return nil
}

// familyCompatible returns nil when an image with imageFam can be installed
// using a profile with profileFam. The rules:
//   - profile "any" accepts any image (profile is OS-agnostic by design).
//   - image with empty family also passes (legacy uploads without metadata).
//   - "rhel" covers Rocky/Alma/RHEL 8+/CentOS Stream; "rhel7" is reserved for
//     the legacy CentOS 7 / RHEL 7 path that needs writeIfcfg fallback.
//   - Cross-family compatibility: kylin ↔ ubuntu/rhel, openeuler ↔ rhel.
//   - exact match otherwise.
func familyCompatible(profileFam, imageFam string) error {
	p := strings.ToLower(strings.TrimSpace(profileFam))
	i := strings.ToLower(strings.TrimSpace(imageFam))
	if p == "" {
		p = "any"
	}
	if p == "any" || i == "" {
		return nil
	}
	if p == i {
		return nil
	}
	// Cross-family compatibility map. kylin V10 is Ubuntu-based; V4/Server
	// is CentOS-based. openEuler is RHEL-based. Allow these pairings so
	// operators can use existing ubuntu/rhel profiles with compatible images.
	type compatEntry struct{ a, b string }
	crossFamily := []compatEntry{
		{"kylin", "ubuntu"},
		{"kylin", "rhel"},
		{"openeuler", "rhel"},
	}
	for _, e := range crossFamily {
		if (p == e.a && i == e.b) || (p == e.b && i == e.a) {
			return nil
		}
	}
	return fmt.Errorf("%w: profile expects %q, image is %q", ErrFamilyMismatch, p, i)
}

// allocateIPFromSubnet picks an unused IPv4 address for machineUUID inside the
// subnet's CIDR. Reservation sources, in order of authority:
//
//   - network / broadcast / gateway
//   - other bindings' static_address
//   - the controller host's own addresses (a controller may not lease itself)
//   - non-expired DHCP leases (same SQLite DB; pool clients hold these)
//   - the configured DHCP pool range when it overlaps this subnet — handing a
//     pool address out statically would let the DHCP server promise it to
//     somebody else later
//
// Surviving candidates are then probed for liveness on the local L2 segment
// (see IPProber): a device that answers ARP owns the address even if it is
// absent from every catalog. Probing is best-effort — when the controller has
// no interface in the subnet (a VLAN it is not attached to) or raw sockets
// are unavailable, allocation proceeds unprobed and says so in the log.
//
// The machine's own previously allocated address is kept ("sticky"): editing
// a binding must not silently move the host to a different IP.
func (s *Store) allocateIPFromSubnet(ctx context.Context, machineUUID, cidr, gateway string) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse CIDR %q: %w", cidr, err)
	}
	network := ipnet.IP.Mask(ipnet.Mask)

	// Sticky: a re-PUT without an explicit address keeps the existing one when
	// it is still inside this subnet and not a special address.
	var own string
	_ = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(static_address, '') FROM bindings WHERE machine_uuid = ?`, machineUUID).
		Scan(&own)
	if own != "" {
		if ip := net.ParseIP(own).To4(); ip != nil && ipnet.Contains(ip) &&
			!ip.Equal(network) && !isBroadcast(ip, ipnet) {
			s.logger.Info("ip allocation: keeping existing address",
				"machine_uuid", machineUUID, "ip", own)
			return own, nil
		}
	}

	used, err := s.reservedIPs(ctx, machineUUID, ipnet, gateway)
	if err != nil {
		return "", err
	}

	candidates := make([]net.IP, 0, 64)
	for ip := nextIP(network); ipnet.Contains(ip); ip = nextIP(ip) {
		if isBroadcast(ip, ipnet) {
			break
		}
		c := ip.String()
		if used[c] {
			continue
		}
		candidates = append(candidates, append(net.IP(nil), ip...))
		if len(candidates) >= maxAllocCandidates {
			break
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no available IP addresses in %s (all %d scanned addresses reserved)",
			cidr, maxAllocCandidates)
	}

	// Probe in batches; the first candidate that stays silent is ours.
	if s.prober != nil {
		probed, skipped := 0, 0
		for i := 0; i < len(candidates) && probed < maxProbeCandidates; i += probeBatchSize {
			end := i + probeBatchSize
			if end > len(candidates) {
				end = len(candidates)
			}
			if end-i > maxProbeCandidates-probed {
				end = i + (maxProbeCandidates - probed)
			}
			batch := candidates[i:end]
			probed += len(batch)
			live, perr := s.prober.InUseBatch(ctx, batch)
			if perr != nil {
				s.logger.Warn("ip allocation: liveness probe unavailable, assigning without probe",
					"subnet", cidr, "err", perr)
				return batch[0].String(), nil
			}
			for _, c := range batch {
				if !live[c.String()] {
					if skipped > 0 {
						s.logger.Info("ip allocation: skipped live addresses",
							"subnet", cidr, "skipped", skipped, "allocated", c.String())
					}
					return c.String(), nil
				}
				skipped++
			}
		}
		if probed >= maxProbeCandidates {
			return "", fmt.Errorf(
				"no free address found in %s after probing %d candidates (all answered ARP); set a static address manually",
				cidr, probed)
		}
		return "", fmt.Errorf("no available IP addresses in %s (every candidate answered ARP)", cidr)
	}
	return candidates[0].String(), nil
}

// Bounds for a single allocation run: candidates to enumerate and how many
// share one probe window. A /24 exhausts far below these; they only matter
// for very large CIDRs where scanning everything would stall the request.
const (
	// maxAllocCandidates bounds how many addresses a single allocation may
	// enumerate (a /24 exhausts far below this; it only matters for large CIDRs).
	maxAllocCandidates = 512
	// probeBatchSize is how many candidates share one ARP window; each window
	// costs ~0.9s, so bigger batches keep worst-case latency down.
	probeBatchSize = 16
	// maxProbeCandidates caps liveness probing. Beyond it the allocator errors
	// out (with the count) instead of stalling: an operator who needs an
	// address in a dense range can set it by hand.
	maxProbeCandidates = 128
)

// reservedIPs collects every address that must not be handed out: gateway,
// other bindings' static addresses, the controller's own addresses, live
// leases, and the DHCP pool range when it overlaps ipnet.
func (s *Store) reservedIPs(ctx context.Context, machineUUID string, ipnet *net.IPNet, gateway string) (map[string]bool, error) {
	used := make(map[string]bool)
	if gateway != "" {
		used[gateway] = true
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT static_address FROM bindings
          WHERE static_address IS NOT NULL AND static_address != '' AND machine_uuid != ?`,
		machineUUID)
	if err != nil {
		return nil, fmt.Errorf("query existing IPs: %w", err)
	}
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan IP: %w", err)
		}
		used[ip] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// The controller's own addresses (serverIP, DHCP/TFTP listener host).
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if v4 := n.IP.To4(); v4 != nil {
					used[v4.String()] = true
				}
			}
		}
	}

	// Leases still in force. Rows live in the same DB (shared connection) so
	// this is a plain read; a missing table (DHCP never ran) is not an error.
	leaseRows, err := s.db.QueryContext(ctx,
		`SELECT ip FROM leases WHERE expires_at > ?`, time.Now().Unix())
	if err != nil {
		if !strings.Contains(err.Error(), "no such table") {
			return nil, fmt.Errorf("query leases: %w", err)
		}
	} else {
		for leaseRows.Next() {
			var ip string
			if err := leaseRows.Scan(&ip); err != nil {
				leaseRows.Close()
				return nil, err
			}
			used[ip] = true
		}
		if err := leaseRows.Err(); err != nil {
			leaseRows.Close()
			return nil, err
		}
		leaseRows.Close()
	}

	// Dynamic pool: skip it when configured on this subnet. Keys match
	// settings.KeyDHCPStart/End — read directly to keep the bindings package
	// free of a settings dependency (values are plain text rows).
	var mode, start, end string
	poolRows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM settings WHERE key IN ('dhcp.mode', 'dhcp.pool.start', 'dhcp.pool.end')`)
	if err != nil {
		if !strings.Contains(err.Error(), "no such table") {
			return nil, fmt.Errorf("query settings: %w", err)
		}
	} else {
		for poolRows.Next() {
			var k, v string
			if err := poolRows.Scan(&k, &v); err != nil {
				poolRows.Close()
				return nil, err
			}
			switch k {
			case "dhcp.mode":
				mode = v
			case "dhcp.pool.start":
				start = v
			case "dhcp.pool.end":
				end = v
			}
		}
		if err := poolRows.Err(); err != nil {
			poolRows.Close()
			return nil, err
		}
		poolRows.Close()
	}
	if mode == "full" {
		if sip, eip := net.ParseIP(start).To4(), net.ParseIP(end).To4(); sip != nil && eip != nil {
			if ipnet.Contains(sip) {
				for ip := sip; ipnet.Contains(ip) && !ipGT(ip, eip); ip = nextIP(ip) {
					used[ip.String()] = true
				}
			}
		}
	}
	return used, nil
}

// ipGT reports whether a > b for two IPv4 addresses.
func ipGT(a, b net.IP) bool {
	return bytes.Compare(a.To4(), b.To4()) > 0
}

// nextIP returns the next IP address
func nextIP(ip net.IP) net.IP {
	next := make(net.IP, len(ip))
	copy(next, ip)
	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] > 0 {
			break
		}
	}
	return next
}

// isBroadcast checks if an IP is the broadcast address for the given network
func isBroadcast(ip net.IP, ipnet *net.IPNet) bool {
	if ip = ip.To4(); ip == nil {
		return false
	}
	broadcast := make(net.IP, len(ip))
	for i := range ip {
		broadcast[i] = ip[i] | ^ipnet.Mask[i]
	}
	return ip.Equal(broadcast)
}
