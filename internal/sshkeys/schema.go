// Package sshkeys is the metalkit SSH public key catalog. A row is one
// reusable operator public key (the "type base64 comment" line from
// ssh-keygen's .pub output). Bindings reference keys by id so the same
// key can be injected into /root/.ssh/authorized_keys on many machines
// without re-pasting it at every install.
package sshkeys

// SQLite schema for the SSH public key catalog. Same naming convention as
// subnets/profiles: 32-char hex id, short human-readable name, UNIQUE.
//
// public_key stores the canonical single-line form "type base64 comment"
// (whitespace-normalised at validate time) so the installer can write it
// into authorized_keys verbatim. key_type/fingerprint are derived columns
// kept alongside for listing/display — fingerprint matches `ssh-keygen -lf`
// output (SHA256, base64 without padding).
const schemaSQL = `
CREATE TABLE IF NOT EXISTS ssh_keys (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    public_key   TEXT NOT NULL,
    key_type     TEXT NOT NULL,
    fingerprint  TEXT NOT NULL,
    comment      TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    created_by   TEXT NOT NULL DEFAULT '',
    updated_by   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_ssh_keys_created_at ON ssh_keys(created_at DESC);
`

// migrations runs idempotent ALTERs for columns added after the initial
// schema. Currently none — the table shipped complete.
var migrations = []string{}
