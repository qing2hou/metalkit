package bindings

// SQLite schema for install bindings. One row per machine — a binding is the
// "what should this machine be running" pointer that admins flip from the UI.
//
// The jobs table (M2.3-5) consumes this: when desired_state moves to
// install/reinstall the orchestrator creates a job carrying the current
// (image_id, profile_id, static_address, hostname) and reboots the host.
// Past assignments are not preserved here — that lives in the jobs history.
//
// Foreign keys are declared so the schema documents intent; runtime
// PRAGMA foreign_keys=on is set by sqlitedb. To survive pragma drift across
// pooled connections we *also* do app-level lookups in store.go before
// each Upsert and gate image/profile deletes through RefCount* helpers.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS bindings (
    machine_uuid       TEXT PRIMARY KEY,
    image_id           TEXT NOT NULL,
    profile_id         TEXT NOT NULL,
    desired_state      TEXT NOT NULL CHECK(desired_state IN ('none','install','reinstall')),
    static_address     TEXT,
    hostname           TEXT,
    root_password_enc  BLOB,
    updated_at         INTEGER NOT NULL,
    updated_by         TEXT NOT NULL,
    FOREIGN KEY (machine_uuid) REFERENCES machines(uuid),
    FOREIGN KEY (image_id)     REFERENCES images(id),
    FOREIGN KEY (profile_id)   REFERENCES profiles(id)
);
CREATE INDEX IF NOT EXISTS idx_bindings_image_id   ON bindings(image_id);
CREATE INDEX IF NOT EXISTS idx_bindings_profile_id ON bindings(profile_id);
`

// migrations runs idempotent ALTERs for columns added after the initial
// schema. SQLite errors on duplicate column add — we swallow that one.
//
// subnet_id (M2.3-12) decouples per-machine network params (gateway, DNS,
// VLAN) from the install profile. When set, the agent renders network
// config from the referenced subnets row, not from profile.network. The
// binding's existing static_address is reinterpreted as the host IP inside
// the chosen subnet. vlan_override (0 = use subnet's VLAN as-is, 1..4094 =
// per-binding tag) covers the edge case of one host needing a different tag
// than the rest of the subnet.
var migrations = []string{
	`ALTER TABLE bindings ADD COLUMN root_password_enc BLOB`,
	`ALTER TABLE bindings ADD COLUMN target_disk_override TEXT`,
	`ALTER TABLE bindings ADD COLUMN bond_override TEXT`,
	`ALTER TABLE bindings ADD COLUMN subnet_id TEXT`,
	`ALTER TABLE bindings ADD COLUMN vlan_override INTEGER`,
	`CREATE INDEX IF NOT EXISTS idx_bindings_subnet_id ON bindings(subnet_id)`,
	// nic_selector_override (per-binding override of profile.network.nic_selector).
	// Empty / NULL = inherit profile. Same one-shot semantics as bond_override and
	// vlan_override: install modal becomes the source of truth, operator picks the
	// concrete physical NIC by MAC at install time.
	`ALTER TABLE bindings ADD COLUMN nic_selector_override TEXT`,
}
