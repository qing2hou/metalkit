package leases

// SQLite schema for DHCP leases handed out by the embedded full-mode DHCP
// server. One row per MAC; (mac) is the primary key and (ip) is UNIQUE so
// the DB itself enforces "no two clients on the same address".
//
// state captures where the row sits in the DHCP handshake:
//   - offered: we replied OFFER; awaiting the client's REQUEST. Held briefly
//     so the same IP isn't handed to a different MAC before the client
//     confirms. Expires_at on offered rows is ~60s in the future.
//   - active:  client sent REQUEST and we replied ACK. expires_at is the
//     real lease deadline (now + leaseHours).
//   - released: client sent RELEASE or we received DECLINE; row kept for
//     audit and to keep the same IP sticky to the same MAC on re-attach.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS leases (
    mac         TEXT PRIMARY KEY,
    ip          TEXT NOT NULL UNIQUE,
    hostname    TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL CHECK(state IN ('offered','active','released')),
    expires_at  INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_leases_expires_at ON leases(expires_at);
CREATE INDEX IF NOT EXISTS idx_leases_ip ON leases(ip);
`
