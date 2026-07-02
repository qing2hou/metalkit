package inventory

// SQLite schema. WAL mode + busy_timeout=5000ms are applied by store.go via PRAGMA.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS machines (
    uuid          TEXT PRIMARY KEY,
    serial        TEXT,
    manufacturer  TEXT,
    product_name  TEXT,
    first_seen    INTEGER NOT NULL,
    last_seen     INTEGER NOT NULL,
    status        TEXT NOT NULL CHECK(status IN ('online','offline','unknown')),
    latest_report INTEGER
);

CREATE TABLE IF NOT EXISTS machine_macs (
    uuid TEXT NOT NULL REFERENCES machines(uuid) ON DELETE CASCADE,
    mac  TEXT NOT NULL,
    role TEXT NOT NULL CHECK(role IN ('bmc','nic')),
    PRIMARY KEY (uuid, mac)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mac ON machine_macs(mac);

CREATE TABLE IF NOT EXISTS reports (
    id    INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid  TEXT NOT NULL REFERENCES machines(uuid) ON DELETE CASCADE,
    ts    INTEGER NOT NULL,
    body  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reports_uuid_ts ON reports(uuid, ts DESC);

CREATE TABLE IF NOT EXISTS heartbeats (
    uuid TEXT NOT NULL REFERENCES machines(uuid) ON DELETE CASCADE,
    ts   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hb_uuid_ts ON heartbeats(uuid, ts DESC);

-- bmc_credentials is owned by the bmc package, but ListMachines JOINs it
-- to surface "managed" status on the machine list. CREATE IF NOT EXISTS
-- keeps this idempotent alongside bmc.NewStore's own schema migration.
CREATE TABLE IF NOT EXISTS bmc_credentials (
    machine_uuid    TEXT PRIMARY KEY NOT NULL,
    ip              TEXT NOT NULL,
    port            INTEGER NOT NULL DEFAULT 623,
    username        TEXT NOT NULL,
    password_ct     BLOB NOT NULL,
    ipmi_interface  TEXT NOT NULL DEFAULT 'lanplus',
    name            TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    updated_by      TEXT NOT NULL
);
`
