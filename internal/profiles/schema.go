package profiles

// SQLite schema for install profile templates. Layered onto the same
// connection as inventory/images; the bindings table (M2.3-2) will FK
// profile_id back into this table.
//
// One row per profile. The mutable JSON columns hold structured config
// (target disk selector + network config); validation lives in validate.go
// and the row never stores anything we haven't already parsed and accepted.
//
// Per-machine state (static IP, hostname overrides) lives in the bindings
// table, not here — a profile is reusable across machines.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS profiles (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    description         TEXT,
    hostname_template   TEXT NOT NULL,
    root_password_hash  TEXT NOT NULL,
    target_disk_json    TEXT NOT NULL,
    network_json        TEXT NOT NULL,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    created_by          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_profiles_created_at ON profiles(created_at DESC);
`
