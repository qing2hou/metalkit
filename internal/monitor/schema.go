package monitor

// SQLite schema for the metrics store. One row per (machine, sample);
// the API handler appends and the retention janitor prunes.
//
// machine_samples is intentionally NOT foreign-keyed to machines(uuid):
// metrics may arrive for a machine the inventory never saw (e.g. the
// monitor was implanted by hand, or the machine row was deleted). The
// query side joins softly — missing machine rows just mean the UI shows
// the UUID without a serial.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS machine_samples (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    machine_uuid TEXT NOT NULL,
    ts           INTEGER NOT NULL,
    payload_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_machine_samples_stream ON machine_samples(machine_uuid, id);
CREATE INDEX IF NOT EXISTS idx_machine_samples_ts ON machine_samples(ts);
`
