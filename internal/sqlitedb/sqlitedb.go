// Package sqlitedb opens a SQLite *sql.DB with the pragmas metalkit relies on
// (WAL, busy_timeout=5s, foreign_keys=on) and the modernc.org/sqlite driver
// registered. Callers own the returned handle and must Close it.
//
// Multiple stores (inventory, images, jobs) layer their own schemas on top of
// the same handle so cross-table foreign keys work and there is only one DB
// file on disk.
package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"

	_ "modernc.org/sqlite"
)

// Options configures Open.
type Options struct {
	Path   string
	Logger *slog.Logger
}

// Open returns a connected *sql.DB. ":memory:" is supported for tests; WAL is
// not applied to in-memory databases and the connection pool is capped at 1 so
// each call sees the same private memory database (modernc.org/sqlite gives
// every connection its own otherwise).
//
// Pragmas are injected via DSN query parameters so every pooled connection
// inherits them. This is critical because busy_timeout and foreign_keys are
// per-connection settings in SQLite — running them once via ExecContext only
// affects the single connection that happens to be checked out, leaving other
// connections in the pool with busy_timeout=0 (immediate SQLITE_BUSY on any
// concurrent write). The install pipeline concurrently writes job stage
// updates while the orchestrator touches bindings and the agent API appends
// log rows, so this is a hot path.
func Open(ctx context.Context, opts Options) (*sql.DB, error) {
	if opts.Logger == nil {
		return nil, errors.New("sqlitedb: Logger is required")
	}
	if opts.Path == "" {
		return nil, errors.New("sqlitedb: Path is required")
	}

	dsn := buildDSN(opts.Path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", opts.Path, err)
	}

	if opts.Path == ":memory:" {
		db.SetMaxOpenConns(1)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping %s: %w", opts.Path, err)
	}

	// journal_mode is a database-level setting (persists in the file header),
	// so a single ExecContext is sufficient. We still set it explicitly to
	// handle the case where an existing DB was created in rollback journal
	// mode and is being reopened by a newer build.
	if opts.Path != ":memory:" {
		if _, err := db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("PRAGMA journal_mode = WAL: %w", err)
		}
	}

	return db, nil
}

// buildDSN returns a modernc.org/sqlite DSN with per-connection pragmas baked
// in via _pragma query parameters. The driver applies each _pragma to every
// new connection before handing it out, which is the only reliable way to
// make busy_timeout and foreign_keys stick across a connection pool.
//
// busy_timeout is set to 30s rather than the historical 5s — install-pipeline
// log writes can land while the orchestrator is mid-transaction on bindings,
// and 5s proved too short on slow disks (SQLITE_BUSY surfaced as HTTP 400 to
// the agent, aborting installs at the grub-install stage).
func buildDSN(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(30000)")
	q.Add("_pragma", "foreign_keys(on)")
	if path != ":memory:" {
		q.Add("_pragma", "journal_mode(WAL)")
	}
	return path + "?" + q.Encode()
}

