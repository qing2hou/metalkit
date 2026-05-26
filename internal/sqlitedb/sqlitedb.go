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
func Open(ctx context.Context, opts Options) (*sql.DB, error) {
	if opts.Logger == nil {
		return nil, errors.New("sqlitedb: Logger is required")
	}
	if opts.Path == "" {
		return nil, errors.New("sqlitedb: Path is required")
	}

	db, err := sql.Open("sqlite", opts.Path)
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

	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	}
	if opts.Path != ":memory:" {
		pragmas = append(pragmas, "PRAGMA journal_mode = WAL")
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}

	return db, nil
}
