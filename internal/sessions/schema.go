package sessions

// SQLite schema for web UI session tokens. One row per active login; rows are
// reaped by GCLoop once expires_at is in the past.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS sessions (
    id           TEXT PRIMARY KEY,
    username     TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);
`

// migrations runs idempotent ALTERs for columns added after the initial
// schema. SQLite errors on duplicate column add — store.go swallows that.
var migrations = []string{
	// none yet — keep the slice present for future ALTERs
}
