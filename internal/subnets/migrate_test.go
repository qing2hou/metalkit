package subnets

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"metalkit/internal/sqlitedb"
)

// TestMigrateLegacyPoolColumns simulates a DB written by the single-range
// build: dhcp_pool_start/end set, pools_json absent. NewStore must promote
// them into pools_json (and the promoted row must surface as DHCPRanges).
func TestMigrateLegacyPoolColumns(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "mig.db")
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{Path: path, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 手工建旧结构表（无 pools_json）
	_, err = db.ExecContext(context.Background(), `CREATE TABLE subnets (
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, description TEXT NOT NULL DEFAULT '',
		cidr TEXT NOT NULL, gateway TEXT NOT NULL, dns_json TEXT NOT NULL DEFAULT '[]',
		vlan_id INTEGER, dhcp_pool_start TEXT, dhcp_pool_end TEXT,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		created_by TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '')`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	_, err = db.ExecContext(context.Background(), `INSERT INTO subnets
		(id,name,cidr,gateway,dns_json,vlan_id,dhcp_pool_start,dhcp_pool_end,created_at,updated_at,created_by,updated_by)
		VALUES ('a','old','192.168.1.0/24','192.168.1.10','[]',10,'192.168.1.100','192.168.1.200',?,?,?,?)`,
		now, now, "tester", "tester")
	if err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(context.Background(), db, logger)
	if err != nil {
		t.Fatalf("NewStore (migration): %v", err)
	}
	sn, err := store.GetByName(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if len(sn.DHCPRanges) != 1 || sn.DHCPRanges[0].Start != "192.168.1.100" || sn.DHCPRanges[0].End != "192.168.1.200" {
		t.Fatalf("migrated DHCPRanges = %+v, want one 192.168.1.100-200", sn.DHCPRanges)
	}
	// 二次 NewStore 幂等（pools_json 已有值不再动）
	if _, err := NewStore(context.Background(), db, logger); err != nil {
		t.Fatalf("second NewStore: %v", err)
	}
	sn2, _ := store.GetByName(context.Background(), "old")
	if len(sn2.DHCPRanges) != 1 {
		t.Fatalf("after re-run DHCPRanges = %+v, still want 1", sn2.DHCPRanges)
	}
}
