package settings

// schemaSQL creates a simple key/value table that the UI uses to persist
// runtime overrides (initially: DHCP pool config). One row per setting key —
// keys are dot-namespaced strings like "dhcp.mode" or "dhcp.pool.start".
// Values are stored as text; callers serialise structured types (lists,
// numbers) themselves so the table stays generic.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL DEFAULT ''
);
`
