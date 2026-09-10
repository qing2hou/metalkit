// Package audit records operator actions into an append-only SQLite table
// so "who did what when" is answerable after the fact. Events are fire-and-
// forget from the caller's perspective: a failed append is logged but never
// fails the operator's request — availability of the control plane beats
// completeness of the audit trail in that trade-off.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type Store struct {
	db     *sql.DB
	logger *slog.Logger
	now    func() time.Time
}

// Event is one audited action. Details is a free-form JSON object with
// action-specific context (ids, names) — never secrets.
type Event struct {
	ID        int64           `json:"id"`
	Timestamp time.Time       `json:"ts"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Target    string          `json:"target,omitempty"`
	Outcome   string          `json:"outcome"` // ok | failed
	Details   json.RawMessage `json:"details,omitempty"`
}

// maxDetailsBytes caps the details payload so a chatty caller can't bloat
// the DB; anything past that is truncated with a marker.
const maxDetailsBytes = 4 * 1024

const schemaSQL = `
CREATE TABLE IF NOT EXISTS audit_events (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    ts       TIMESTAMP NOT NULL,
    actor    TEXT      NOT NULL,
    action   TEXT      NOT NULL,
    target   TEXT      NOT NULL DEFAULT '',
    outcome  TEXT      NOT NULL DEFAULT 'ok',
    details  TEXT      NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_ts    ON audit_events (ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_events (actor, ts DESC);
`

func NewStore(ctx context.Context, db *sql.DB, logger *slog.Logger) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("audit: db is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("audit: logger is required")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply audit schema: %w", err)
	}
	return &Store{db: db, logger: logger, now: time.Now}, nil
}

// Record appends one event. Never returns an error to the caller — a
// failed audit write is logged and the operator request proceeds.
func (s *Store) Record(ctx context.Context, actor, action, target, outcome string, details any) {
	if details == nil {
		details = struct{}{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		s.logger.Warn("audit: marshal details", "action", action, "err", err)
		raw = []byte(`{}`)
	}
	if len(raw) > maxDetailsBytes {
		raw = append(raw[:maxDetailsBytes], []byte(`,"_truncated":true}`)...)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO audit_events (ts, actor, action, target, outcome, details) VALUES (?, ?, ?, ?, ?, ?)`,
		s.now().UTC(), actor, action, target, outcome, string(raw))
	if err != nil {
		// fire-and-forget: log loudly, don't fail the caller's request
		s.logger.Error("audit: append failed", "actor", actor, "action", action, "err", err)
	}
}

// ListOptions filters the query API. Zero value = newest 200 events.
type ListOptions struct {
	Actor  string
	Action string
	Limit  int
	Offset int
}

func (o ListOptions) clamp() ListOptions {
	if o.Limit <= 0 {
		o.Limit = 200
	}
	if o.Limit > 500 {
		o.Limit = 500
	}
	if o.Offset < 0 {
		o.Offset = 0
	}
	return o
}

func (s *Store) List(ctx context.Context, opts ListOptions) ([]Event, error) {
	opts = opts.clamp()
	var sb strings.Builder
	sb.WriteString(`SELECT id, ts, actor, action, target, outcome, details FROM audit_events WHERE 1=1`)
	args := []any{}
	if opts.Actor != "" {
		sb.WriteString(` AND actor = ?`)
		args = append(args, opts.Actor)
	}
	if opts.Action != "" {
		sb.WriteString(` AND action = ?`)
		args = append(args, opts.Action)
	}
	sb.WriteString(` ORDER BY id DESC LIMIT ? OFFSET ?`)
	args = append(args, opts.Limit, opts.Offset)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("audit list: %w", err)
	}
	defer rows.Close()

	out := []Event{}
	for rows.Next() {
		var e Event
		var details string
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Actor, &e.Action, &e.Target, &e.Outcome, &details); err != nil {
			return nil, fmt.Errorf("audit scan: %w", err)
		}
		if details != "" {
			e.Details = json.RawMessage(details)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
