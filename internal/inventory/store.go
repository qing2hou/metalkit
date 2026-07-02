// Package inventory persists agent inventory reports in SQLite and exposes
// an HTTP API for ingestion and queries. NewStore applies the inventory
// schema to a *sql.DB owned by the caller; api.go exposes the store over HTTP.
package inventory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ErrNotFound is returned when a machine, report, or MAC lookup misses.
var ErrNotFound = errors.New("inventory: not found")

// offlineAfter is how long without contact before a machine is marked offline.
// markOfflineTick is how often RunOfflineMarker re-evaluates.
const (
	offlineAfter    = 90 * time.Second
	markOfflineTick = 60 * time.Second
)

// Store is the inventory repository backed by SQLite. The *sql.DB is borrowed
// from the caller (typically opened via sqlitedb.Open and shared with other
// stores like images), and the store does not close it.
type Store struct {
	db          *sql.DB
	logger      *slog.Logger
	bmcReconcil BMCReconciler // optional; unset → no reconciliation
}

// BMCReconciler is satisfied by *bmc.Store. When an agent report arrives with
// BMC.IP set, UpsertReport calls ReconcilePlaceholder(real_uuid, bmc_ip) so a
// pre-existing placeholder credential gets migrated to the real machine row.
// Decoupled via interface so the inventory package doesn't import bmc.
type BMCReconciler interface {
	ReconcilePlaceholder(ctx context.Context, realUUID, bmcIP string) error
}

// WithBMCReconciler wires the BMC store so UpsertReport can migrate placeholder
// credentials. Returns the store so callers can chain.
func (s *Store) WithBMCReconciler(r BMCReconciler) *Store {
	s.bmcReconcil = r
	return s
}

// NewStore applies the inventory schema to db and returns a Store that reads
// and writes through it. The caller retains ownership of db.
func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		return nil, errors.New("inventory: logger is required")
	}
	if db == nil {
		return nil, errors.New("inventory: db is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply inventory schema: %w", err)
	}
	return &Store{db: db, logger: logger}, nil
}

// UpsertReport persists a Report, refreshing identity columns on the machine,
// replacing its MAC set, and appending a new historical report row. All in
// a single transaction so a partial write never leaves a dangling latest_report.
func (s *Store) UpsertReport(ctx context.Context, r *Report) (string, int64, error) {
	if r == nil {
		return "", 0, errors.New("inventory: nil report")
	}
	uuid := strings.ToLower(strings.TrimSpace(r.Machine.SMBIOSUUID))
	if uuid == "" {
		return "", 0, errors.New("inventory: report has empty smbios_uuid")
	}

	body, err := json.Marshal(r)
	if err != nil {
		return "", 0, fmt.Errorf("marshal report: %w", err)
	}

	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// INSERT-or-UPDATE the machine. first_seen is preserved on update.
	_, err = tx.ExecContext(ctx, `
        INSERT INTO machines (uuid, serial, manufacturer, product_name, first_seen, last_seen, status)
        VALUES (?, ?, ?, ?, ?, ?, 'online')
        ON CONFLICT(uuid) DO UPDATE SET
            serial       = excluded.serial,
            manufacturer = excluded.manufacturer,
            product_name = excluded.product_name,
            last_seen    = excluded.last_seen,
            status       = 'online'
    `, uuid, r.Machine.Serial, r.Machine.Manufacturer, r.Machine.ProductName, now, now)
	if err != nil {
		return "", 0, fmt.Errorf("upsert machine: %w", err)
	}

	// Replace the MAC set. Cheaper than diffing for the report cadence we expect.
	if _, err := tx.ExecContext(ctx, `DELETE FROM machine_macs WHERE uuid = ?`, uuid); err != nil {
		return "", 0, fmt.Errorf("clear macs: %w", err)
	}
	seen := make(map[string]struct{})
	insertMAC := func(mac, role string) error {
		mac = normalizeMAC(mac)
		if mac == "" {
			return nil
		}
		if _, dup := seen[mac]; dup {
			return nil
		}
		seen[mac] = struct{}{}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO machine_macs (uuid, mac, role) VALUES (?, ?, ?)`,
			uuid, mac, role); err != nil {
			return fmt.Errorf("insert mac %s: %w", mac, err)
		}
		return nil
	}
	if r.BMC != nil {
		if err := insertMAC(r.BMC.MAC, "bmc"); err != nil {
			return "", 0, err
		}
	}
	for _, n := range r.NICs {
		if err := insertMAC(n.MAC, "nic"); err != nil {
			return "", 0, err
		}
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO reports (uuid, ts, body) VALUES (?, ?, ?)`,
		uuid, now, string(body))
	if err != nil {
		return "", 0, fmt.Errorf("insert report: %w", err)
	}
	reportID, err := res.LastInsertId()
	if err != nil {
		return "", 0, fmt.Errorf("last insert id: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE machines SET latest_report = ? WHERE uuid = ?`,
		reportID, uuid); err != nil {
		return "", 0, fmt.Errorf("set latest_report: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("commit: %w", err)
	}

	// Best-effort reconciliation: if a placeholder BMC credential was registered
	// for this machine's BMC IP before it PXE-reported, migrate the credential
	// over to the real SMBIOS UUID and delete the placeholder. Runs outside the
	// inventory tx because (a) bmc.Store owns its own tx and (b) the FK from
	// bmc_credentials → machines(uuid) requires the real machine row to be
	// visible before we can UPDATE the credential's machine_uuid.
	if s.bmcReconcil != nil && r.BMC != nil && r.BMC.IP != "" {
		if err := s.bmcReconcil.ReconcilePlaceholder(ctx, uuid, r.BMC.IP); err != nil {
			s.logger.Warn("bmc placeholder reconcile failed",
				"uuid", uuid, "bmc_ip", r.BMC.IP, "err", err)
		}
	}
	return uuid, reportID, nil
}

// Heartbeat advances last_seen and inserts a heartbeats row. It does not
// create machines; an unknown uuid returns ErrNotFound so callers can
// distinguish a missed registration from a transient error.
func (s *Store) Heartbeat(ctx context.Context, uuid string) error {
	uuid = strings.ToLower(strings.TrimSpace(uuid))
	if uuid == "" {
		return errors.New("inventory: empty uuid")
	}
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE machines SET last_seen = ?, status = 'online' WHERE uuid = ?`,
		now, uuid)
	if err != nil {
		return fmt.Errorf("update machine: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO heartbeats (uuid, ts) VALUES (?, ?)`, uuid, now); err != nil {
		return fmt.Errorf("insert heartbeat: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Delete removes the machine row. Cascading FKs wipe machine_macs, reports,
// heartbeats, bmc_credentials, and jobs. Callers are expected to have already
// deleted the binding row (it FK-refs machines without CASCADE) and to have
// confirmed no active job is running on this UUID.
func (s *Store) Delete(ctx context.Context, uuid string) error {
	uuid = strings.ToLower(strings.TrimSpace(uuid))
	if uuid == "" {
		return errors.New("inventory: empty uuid")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM machines WHERE uuid = ?`, uuid)
	if err != nil {
		return fmt.Errorf("delete machine: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MachineSummary is one row of the /machines listing.
type MachineSummary struct {
	UUID         string    `json:"uuid"`
	Serial       string    `json:"serial"`
	Manufacturer string    `json:"manufacturer"`
	ProductName  string    `json:"product_name"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	Status       string    `json:"status"`
	LatestReport int64     `json:"latest_report"`
	BMCIP        string    `json:"bmc_ip"`       // agent-reported BMC IP, parsed from latest report's JSON body
	BMCManaged   bool      `json:"bmc_managed"`  // true if bmc_credentials row exists for this machine
}

// ListMachines returns all machines, most-recently-seen first.
func (s *Store) ListMachines(ctx context.Context) ([]MachineSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT m.uuid, m.serial, m.manufacturer, m.product_name,
               m.first_seen, m.last_seen, m.status,
               COALESCE(m.latest_report, 0),
               COALESCE(json_extract(r.body, '$.bmc.ip'), ''),
               EXISTS(SELECT 1 FROM bmc_credentials b WHERE b.machine_uuid = m.uuid)
        FROM machines m
        LEFT JOIN reports r ON r.id = m.latest_report
        ORDER BY m.last_seen DESC
    `)
	if err != nil {
		return nil, fmt.Errorf("query machines: %w", err)
	}
	defer rows.Close()

	out := make([]MachineSummary, 0)
	for rows.Next() {
		var (
			m                    MachineSummary
			firstSeen, lastSeen  int64
			serial, mfr, product sql.NullString
			bmcIP                sql.NullString
			managed              bool
		)
		if err := rows.Scan(&m.UUID, &serial, &mfr, &product, &firstSeen, &lastSeen,
			&m.Status, &m.LatestReport, &bmcIP, &managed); err != nil {
			return nil, fmt.Errorf("scan machine: %w", err)
		}
		m.Serial = serial.String
		m.Manufacturer = mfr.String
		m.ProductName = product.String
		m.FirstSeen = time.Unix(firstSeen, 0).UTC()
		m.LastSeen = time.Unix(lastSeen, 0).UTC()
		m.BMCIP = bmcIP.String
		m.BMCManaged = managed
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate machines: %w", err)
	}
	return out, nil
}

// LatestReport returns the most recent Report for a machine, or ErrNotFound.
func (s *Store) LatestReport(ctx context.Context, uuid string) (*Report, error) {
	uuid = strings.ToLower(strings.TrimSpace(uuid))
	var body string
	err := s.db.QueryRowContext(ctx, `
        SELECT r.body
        FROM machines m
        JOIN reports  r ON r.id = m.latest_report
        WHERE m.uuid = ?
    `, uuid).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query latest report: %w", err)
	}
	return decodeReport(body)
}

// ReportMeta is one row of the per-machine reports index.
type ReportMeta struct {
	ID int64     `json:"id"`
	TS time.Time `json:"ts"`
}

// ListReports returns the historical reports for a machine, newest first.
// An unknown uuid returns an empty slice; callers query /machines/{uuid} first
// if they need to distinguish "no reports yet" from "no such machine".
func (s *Store) ListReports(ctx context.Context, uuid string) ([]ReportMeta, error) {
	uuid = strings.ToLower(strings.TrimSpace(uuid))
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts FROM reports WHERE uuid = ? ORDER BY ts DESC, id DESC`, uuid)
	if err != nil {
		return nil, fmt.Errorf("query reports: %w", err)
	}
	defer rows.Close()

	out := make([]ReportMeta, 0)
	for rows.Next() {
		var m ReportMeta
		var ts int64
		if err := rows.Scan(&m.ID, &ts); err != nil {
			return nil, fmt.Errorf("scan report meta: %w", err)
		}
		m.TS = time.Unix(ts, 0).UTC()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reports: %w", err)
	}
	return out, nil
}

// GetReport fetches a specific historical report. id must belong to uuid.
func (s *Store) GetReport(ctx context.Context, uuid string, id int64) (*Report, error) {
	uuid = strings.ToLower(strings.TrimSpace(uuid))
	var body string
	err := s.db.QueryRowContext(ctx,
		`SELECT body FROM reports WHERE id = ? AND uuid = ?`, id, uuid).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query report: %w", err)
	}
	return decodeReport(body)
}

// MacMatch is the result of a LookupByMAC call.
type MacMatch struct {
	UUID string `json:"uuid"`
	Role string `json:"role"`
}

// LookupByMAC resolves a single MAC (NIC or BMC) to the owning machine.
func (s *Store) LookupByMAC(ctx context.Context, mac string) (*MacMatch, error) {
	mac = normalizeMAC(mac)
	if mac == "" {
		return nil, ErrNotFound
	}
	var m MacMatch
	err := s.db.QueryRowContext(ctx,
		`SELECT uuid, role FROM machine_macs WHERE mac = ?`, mac).Scan(&m.UUID, &m.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query mac: %w", err)
	}
	return &m, nil
}

// RunOfflineMarker periodically flips machines to status=offline once they
// haven't been heard from for longer than offlineAfter. The loop exits when
// ctx is cancelled.
func (s *Store) RunOfflineMarker(ctx context.Context) {
	t := time.NewTicker(markOfflineTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.markOfflineOnce(ctx); err != nil {
				s.logger.Warn("offline marker tick failed", "err", err)
			} else if n > 0 {
				s.logger.Info("offline marker", "transitioned", n)
			}
		}
	}
}

// markOfflineOnce performs a single pass; broken out for tests.
func (s *Store) markOfflineOnce(ctx context.Context) (int64, error) {
	cutoff := time.Now().Add(-offlineAfter).Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE machines SET status = 'offline' WHERE last_seen < ? AND status != 'offline'`,
		cutoff)
	if err != nil {
		return 0, fmt.Errorf("mark offline: %w", err)
	}
	return res.RowsAffected()
}

// normalizeMAC lowercases and strips surrounding whitespace. Empty in, empty out.
// We do not validate format here — the api layer does that; the store accepts
// whatever the agent reported so a malformed entry can be surfaced via lookup.
func normalizeMAC(mac string) string {
	return strings.ToLower(strings.TrimSpace(mac))
}

func decodeReport(body string) (*Report, error) {
	var r Report
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return nil, fmt.Errorf("decode report: %w", err)
	}
	return &r, nil
}
