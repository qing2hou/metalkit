// Package leases is the persistent DHCP lease store for metalkit's
// embedded full-mode DHCP server. Each row maps a client MAC to the IPv4
// address we've handed out (or offered), the lease state, and the lease
// deadline.
//
// The store is intentionally small — DHCP itself is the source of truth for
// when a lease should be renewed or recycled, and the store just records
// the current view so leases survive a controller restart.
//
// Allocation policy:
//   - Sticky-by-MAC: a returning client gets the same IP it had before, as
//     long as the row hasn't been overwritten by another MAC.
//   - Lowest-free first: if the MAC is new, we scan the configured pool
//     range from start→end and hand out the first IP not present in the
//     leases table (and not in the configurer's exclude set).
//   - Expired rows are kept until GC runs; their IPs are eligible for
//     re-allocation because the SQLite UNIQUE constraint forces an UPDATE
//     when we re-use that IP.
package leases

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"
)

var (
	ErrNotFound  = errors.New("leases: not found")
	ErrPoolEmpty = errors.New("leases: pool exhausted")
)

type Store struct {
	db     *sql.DB
	logger *slog.Logger
	now    func() time.Time
}

func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger) (*Store, error) {
	if db == nil {
		return nil, errors.New("leases: db is required")
	}
	if logger == nil {
		return nil, errors.New("leases: logger is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply leases schema: %w", err)
	}
	return &Store{db: db, logger: logger, now: time.Now}, nil
}

// Lease is the public view of a row. State is "offered" | "active" |
// "released". IP is canonical IPv4 dotted-quad.
type Lease struct {
	MAC       string
	IP        string
	Hostname  string
	State     string
	ExpiresAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AllocateInput is what dhcp.handler passes to Allocate. Pool bounds are
// passed per call (rather than stored on the Store) so the operator can
// edit config.yaml and reload without a migration step — and so tests can
// vary pools cheaply.
type AllocateInput struct {
	MAC      string
	Hostname string
	Start    netip.Addr
	End      netip.Addr
	Exclude  map[string]struct{} // canonical IPv4 strings
	LeaseDur time.Duration
}

// Allocate picks an IP for the given MAC and writes an "offered" row. The
// returned lease has state="offered" and a short expires_at (1 minute) —
// the caller must follow up with Confirm() on REQUEST/ACK to extend it.
//
// Sticky behaviour: if we already have a row for the MAC whose IP is still
// inside the pool and not excluded, we reuse it (refresh expires_at). This
// is what makes a reboot keep the same address.
func (s *Store) Allocate(ctx context.Context, in AllocateInput) (*Lease, error) {
	if in.MAC == "" {
		return nil, errors.New("leases: mac required")
	}
	if !in.Start.Is4() || !in.End.Is4() {
		return nil, errors.New("leases: pool bounds must be IPv4")
	}
	if in.Start.Compare(in.End) > 0 {
		return nil, fmt.Errorf("leases: start %s > end %s", in.Start, in.End)
	}
	if in.LeaseDur <= 0 {
		in.LeaseDur = 24 * time.Hour
	}
	offerDur := time.Minute

	now := s.now().UTC()
	mac := normalizeMAC(in.MAC)

	// Sticky path: existing row for this MAC, IP still in range and not excluded.
	if existing, err := s.getByMAC(ctx, mac); err == nil {
		if ipInRange(existing.IP, in.Start, in.End) && !excluded(in.Exclude, existing.IP) {
			expires := now.Add(offerDur)
			if _, err := s.db.ExecContext(ctx, `
                UPDATE leases SET state='offered', expires_at=?, updated_at=?, hostname=?
                WHERE mac=?`,
				expires.Unix(), now.Unix(), in.Hostname, mac,
			); err != nil {
				return nil, fmt.Errorf("refresh sticky lease: %w", err)
			}
			existing.State = "offered"
			existing.ExpiresAt = expires
			existing.UpdatedAt = now
			existing.Hostname = in.Hostname
			return existing, nil
		}
		// Sticky IP no longer suitable (pool changed, exclude grew): drop and re-pick.
		if _, err := s.db.ExecContext(ctx, `DELETE FROM leases WHERE mac=?`, mac); err != nil {
			return nil, fmt.Errorf("drop stale sticky: %w", err)
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// Build the set of IPs that are currently taken: any row whose lease
	// hasn't expired AND whose IP falls in the pool. Expired rows are fair
	// game because we'll UPSERT and clobber them.
	taken, err := s.takenSet(ctx, now)
	if err != nil {
		return nil, err
	}

	chosen, ok := nextFree(in.Start, in.End, in.Exclude, taken)
	if !ok {
		return nil, ErrPoolEmpty
	}

	expires := now.Add(offerDur)
	// Upsert by IP first to evict any expired holder of the same IP, then
	// upsert by MAC to anchor the new row. Two statements keeps the SQL
	// portable and the intent obvious.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM leases WHERE ip=?`, chosen.String()); err != nil {
		return nil, fmt.Errorf("evict prior holder of %s: %w", chosen, err)
	}
	if _, err := s.db.ExecContext(ctx, `
        INSERT INTO leases (mac, ip, hostname, state, expires_at, created_at, updated_at)
        VALUES (?, ?, ?, 'offered', ?, ?, ?)
        ON CONFLICT(mac) DO UPDATE SET
            ip=excluded.ip, hostname=excluded.hostname, state='offered',
            expires_at=excluded.expires_at, updated_at=excluded.updated_at`,
		mac, chosen.String(), in.Hostname, expires.Unix(), now.Unix(), now.Unix(),
	); err != nil {
		return nil, fmt.Errorf("insert lease: %w", err)
	}
	return &Lease{
		MAC: mac, IP: chosen.String(), Hostname: in.Hostname,
		State: "offered", ExpiresAt: expires, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// Confirm transitions a lease from "offered" to "active" and extends
// expires_at by leaseDur. It rejects a REQUEST for an IP that doesn't
// match our offered row — that case is what the DHCP layer maps to NAK.
func (s *Store) Confirm(ctx context.Context, mac, requestedIP string, leaseDur time.Duration) (*Lease, error) {
	mac = normalizeMAC(mac)
	row, err := s.getByMAC(ctx, mac)
	if err != nil {
		return nil, err
	}
	if requestedIP != "" && row.IP != requestedIP {
		return nil, fmt.Errorf("leases: requested %s but offered %s", requestedIP, row.IP)
	}
	now := s.now().UTC()
	expires := now.Add(leaseDur)
	if _, err := s.db.ExecContext(ctx, `
        UPDATE leases SET state='active', expires_at=?, updated_at=? WHERE mac=?`,
		expires.Unix(), now.Unix(), mac,
	); err != nil {
		return nil, fmt.Errorf("confirm lease: %w", err)
	}
	row.State = "active"
	row.ExpiresAt = expires
	row.UpdatedAt = now
	return row, nil
}

// Release marks a lease as released. The row is kept (not deleted) so the
// same client gets the same IP back next time, but the IP is immediately
// eligible for re-allocation to another client.
func (s *Store) Release(ctx context.Context, mac string) error {
	mac = normalizeMAC(mac)
	now := s.now().UTC()
	res, err := s.db.ExecContext(ctx, `
        UPDATE leases SET state='released', expires_at=?, updated_at=? WHERE mac=?`,
		now.Unix(), now.Unix(), mac,
	)
	if err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GC removes lease rows whose expires_at is more than `keepFor` in the past.
// Active leases past their deadline are dropped (the client either renewed
// or vanished); released rows are dropped after the same grace period.
func (s *Store) GC(ctx context.Context, keepFor time.Duration) (int, error) {
	cutoff := s.now().UTC().Add(-keepFor).Unix()
	res, err := s.db.ExecContext(ctx, `DELETE FROM leases WHERE expires_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("gc leases: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// List returns all rows in deterministic order — used by debug endpoints.
func (s *Store) List(ctx context.Context) ([]Lease, error) {
	rows, err := s.db.QueryContext(ctx, selectSQL+` ORDER BY ip ASC`)
	if err != nil {
		return nil, fmt.Errorf("list leases: %w", err)
	}
	defer rows.Close()
	out := make([]Lease, 0)
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// ---- internal helpers ----

const selectSQL = `SELECT mac, ip, hostname, state, expires_at, created_at, updated_at FROM leases`

func (s *Store) getByMAC(ctx context.Context, mac string) (*Lease, error) {
	row := s.db.QueryRowContext(ctx, selectSQL+` WHERE mac=?`, mac)
	return scanLease(row)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanLease(r rowScanner) (*Lease, error) {
	var l Lease
	var expires, created, updated int64
	if err := r.Scan(&l.MAC, &l.IP, &l.Hostname, &l.State, &expires, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan lease: %w", err)
	}
	l.ExpiresAt = time.Unix(expires, 0).UTC()
	l.CreatedAt = time.Unix(created, 0).UTC()
	l.UpdatedAt = time.Unix(updated, 0).UTC()
	return &l, nil
}

// takenSet returns the IPs currently held by an unexpired lease (either
// offered or active). Released rows are NOT taken — their IPs are reusable.
func (s *Store) takenSet(ctx context.Context, now time.Time) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT ip FROM leases WHERE state IN ('offered','active') AND expires_at >= ?`,
		now.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("query taken: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		out[ip] = struct{}{}
	}
	return out, rows.Err()
}

// nextFree scans [start,end] inclusive and returns the lowest IP that's
// neither in `taken` nor in `exclude`. The scan is bounded by the pool
// size which in practice is at most a few thousand addresses — cheap.
func nextFree(start, end netip.Addr, exclude, taken map[string]struct{}) (netip.Addr, bool) {
	for ip := start; ip.Compare(end) <= 0; ip = ip.Next() {
		s := ip.String()
		if _, bad := exclude[s]; bad {
			continue
		}
		if _, bad := taken[s]; bad {
			continue
		}
		return ip, true
	}
	return netip.Addr{}, false
}

func ipInRange(ip string, start, end netip.Addr) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	return a.Compare(start) >= 0 && a.Compare(end) <= 0
}

func excluded(ex map[string]struct{}, ip string) bool {
	_, bad := ex[ip]
	return bad
}

// normalizeMAC lowercases a MAC string so 00:11:22:aa:BB:CC and
// 00:11:22:AA:bb:cc collapse to a single primary key value.
func normalizeMAC(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
