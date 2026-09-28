// store.go persists metrics samples and serves them back for the UI.
package monitor

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

// Retention config. Samples are tiny (a few hundred bytes) but they arrive
// every interval from every machine forever — without pruning the table
// grows unbounded. 14 days at 60s cadence × 100 machines ≈ 20M rows is
// too much; the UI only ever shows recent windows, so 7 days is generous.
const (
	DefaultRetention = 7 * 24 * time.Hour
	// JanitorInterval is how often Prune runs when driven by the GC loop.
	JanitorInterval = time.Hour
)

// Store reads and writes machine_samples rows.
type Store struct {
	db     *sql.DB
	logger *slog.Logger
	now    func() time.Time
}

// NewStore applies the monitor schema.
func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger) (*Store, error) {
	if db == nil {
		return nil, errors.New("monitor: db is required")
	}
	if logger == nil {
		return nil, errors.New("monitor: logger is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply monitor schema: %w", err)
	}
	return &Store{db: db, logger: logger, now: time.Now}, nil
}

// Append stores one sample. uuid must be non-empty (shape-checked here;
// whether the machine exists in inventory is not this layer's concern).
func (s *Store) Append(ctx context.Context, uuid string, p Payload) error {
	uuid = strings.ToLower(strings.TrimSpace(uuid))
	if uuid == "" || uuid != strings.ToLower(strings.TrimSpace(p.MachineUUID)) {
		return errors.New("monitor: machine_uuid mismatch or empty")
	}
	blob, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("monitor: marshal payload: %w", err)
	}
	ts := p.MonitoredAt.Unix()
	if ts == 0 {
		ts = s.now().Unix()
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO machine_samples (machine_uuid, ts, payload_json)
        VALUES (?, ?, ?)`, uuid, ts, string(blob))
	if err != nil {
		return fmt.Errorf("monitor: append sample: %w", err)
	}
	return nil
}

// Latest returns the most recent sample per machine (uuid → payload).
// Only machines with at least one sample in `within` show up, so the UI
// can treat absence as "monitor not reporting".
func (s *Store) Latest(ctx context.Context, within time.Duration) (map[string]Payload, error) {
	cutoff := s.now().Add(-within).Unix()
	rows, err := s.db.QueryContext(ctx, `
        SELECT machine_uuid, payload_json FROM machine_samples s
        WHERE id IN (SELECT MAX(id) FROM machine_samples GROUP BY machine_uuid)
          AND ts >= ?`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("monitor: latest: %w", err)
	}
	defer rows.Close()
	out := map[string]Payload{}
	for rows.Next() {
		var uuid, blob string
		if err := rows.Scan(&uuid, &blob); err != nil {
			return nil, fmt.Errorf("monitor: latest scan: %w", err)
		}
		var p Payload
		if err := json.Unmarshal([]byte(blob), &p); err != nil {
			continue // corrupt row — skip rather than fail the listing
		}
		out[uuid] = p
	}
	return out, rows.Err()
}

// History returns up to limit samples for one machine, oldest first.
func (s *Store) History(ctx context.Context, uuid string, limit int) ([]Payload, error) {
	uuid = strings.ToLower(strings.TrimSpace(uuid))
	if limit <= 0 {
		limit = 60
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT payload_json FROM (
            SELECT payload_json, id FROM machine_samples
            WHERE machine_uuid = ? ORDER BY id DESC LIMIT ?
        ) ORDER BY id ASC`, uuid, limit)
	if err != nil {
		return nil, fmt.Errorf("monitor: history: %w", err)
	}
	defer rows.Close()
	out := []Payload{}
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return nil, fmt.Errorf("monitor: history scan: %w", err)
		}
		var p Payload
		if err := json.Unmarshal([]byte(blob), &p); err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Prune deletes samples older than the retention window. Returns the
// number of rows deleted. Idempotent — safe to call on a timer.
func (s *Store) Prune(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		retention = DefaultRetention
	}
	cutoff := s.now().Add(-retention).Unix()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM machine_samples WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("monitor: prune: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GCLoop prunes on a timer until ctx is cancelled. Forgets per-run errors
// (logged) — retention is housekeeping, never fatal.
func (s *Store) GCLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = JanitorInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.Prune(ctx, DefaultRetention)
			if err != nil {
				s.logger.Warn("monitor: prune failed", "err", err)
			} else if n > 0 {
				s.logger.Info("monitor: pruned samples", "count", n)
			}
		}
	}
}
