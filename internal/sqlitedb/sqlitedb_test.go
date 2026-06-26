package sqlitedb

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func TestOpenAppliesPragmas(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := Open(context.Background(), Options{
		Path:   filepath.Join(t.TempDir(), "test.db"),
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode: got %q want wal", mode)
	}
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys: got %d want 1", fk)
	}
	var bt int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&bt); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if bt != 30000 {
		t.Fatalf("busy_timeout: got %d want 30000", bt)
	}
}

func TestOpenInMemoryNoWAL(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := Open(context.Background(), Options{Path: ":memory:", Logger: logger})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	// In-memory cannot use WAL; modernc.org/sqlite reports "memory".
	if mode == "wal" {
		t.Fatalf("in-memory should not be in WAL mode, got %q", mode)
	}

	// Single connection: two statements see the same data.
	if _, err := db.Exec(`CREATE TABLE t(x INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("count: got %d want 1", n)
	}
}

func TestOpenRejectsEmpty(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := Open(context.Background(), Options{Logger: logger}); err == nil {
		t.Fatal("expected error for empty Path")
	}
	if _, err := Open(context.Background(), Options{Path: ":memory:"}); err == nil {
		t.Fatal("expected error for nil Logger")
	}
}
