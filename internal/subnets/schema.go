package subnets

// SQLite schema for the subnets catalog. A subnet captures the L3 attributes
// shared by many machines on the same network segment: CIDR, gateway, DNS
// servers, and optional 802.1Q VLAN tag. Bindings reference a subnet by id
// and then add per-machine fields (host IP, bond config) on top.
//
// pools_json (optional): JSON array of {"start","end"} address ranges,
// e.g. [{"start":"192.168.1.100","end":"192.168.1.150"},
//
//	{"start":"192.168.1.180","end":"192.168.1.200"}]. When non-empty,
//
// full-mode DHCP serves relays from this subnet out of these ranges — the
// relay's giaddr (the VLAN SVI) matches the CIDR and picks the pool. Empty
// means the subnet is usable for static installs only and DHCP requests
// relayed from it go unanswered.
//
// dhcp_pool_start/dhcp_pool_end are the pre-multi-range columns, kept only
// so NewStore can migrate them into pools_json once; nothing writes them
// after that.
//
// Naming convention is the same as profiles/bindings: 32-char hex id, short
// human-readable name with UNIQUE constraint.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS subnets (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    description  TEXT NOT NULL DEFAULT '',
    cidr         TEXT NOT NULL,
    gateway      TEXT NOT NULL,
    dns_json     TEXT NOT NULL DEFAULT '[]',
    vlan_id      INTEGER,
    dhcp_pool_start TEXT,
    dhcp_pool_end   TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    created_by   TEXT NOT NULL DEFAULT '',
    updated_by   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_subnets_created_at ON subnets(created_at DESC);
`

// migrations runs idempotent ALTERs for columns added after the initial
// schema. SQLite errors on duplicate column add — we swallow exactly that
// error, matching the bindings/bmc migration pattern.
var migrations = []string{
	`ALTER TABLE subnets ADD COLUMN dhcp_pool_start TEXT`,
	`ALTER TABLE subnets ADD COLUMN dhcp_pool_end TEXT`,
	`ALTER TABLE subnets ADD COLUMN pools_json TEXT`,
}
