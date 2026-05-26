package bmc

// schemaSQL applies the bmc_credentials schema.
//
// The table is keyed by machine_uuid (1:1 with machines) — we don't support
// multiple BMC endpoints per machine. password_ct holds the AES-GCM blob
// produced by internal/crypto; never store plaintext here.
//
// ipmi_interface maps directly to ipmitool's `-I` flag. We only support
// `lan` (legacy IPMI 1.5) and `lanplus` (IPMI 2.0, the modern default).
//
// CHECK constraints stay narrow and serve as a defense-in-depth backstop —
// the Go validators in validate.go are the primary gate.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS bmc_credentials (
    machine_uuid    TEXT PRIMARY KEY NOT NULL
                        REFERENCES machines(uuid) ON DELETE CASCADE,
    ip              TEXT NOT NULL,
    port            INTEGER NOT NULL DEFAULT 623 CHECK (port BETWEEN 1 AND 65535),
    username        TEXT NOT NULL,
    password_ct     BLOB NOT NULL,
    ipmi_interface  TEXT NOT NULL DEFAULT 'lanplus'
                        CHECK (ipmi_interface IN ('lan','lanplus')),
    name            TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    updated_by      TEXT NOT NULL
);
`

// migrations runs idempotent ALTERs for columns added after the initial
// schema. SQLite errors on duplicate column add — we swallow that one.
var migrations = []string{
	`ALTER TABLE bmc_credentials ADD COLUMN name TEXT`,
}
