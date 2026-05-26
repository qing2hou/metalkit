package jobs

// SQLite schema for install/reinstall jobs and their log streams. Tables:
//
//   jobs       — one row per install attempt; lifetime row, terminal states
//                are succeeded/failed/cancelled.
//   job_logs   — append-only line stream from the agent, used by the UI for
//                live tailing (SSE) and post-mortem.
//
// State machine (enforced in code):
//
//     pending → running → succeeded
//                       ↘ failed
//                       ↘ cancelled
//     pending           ↘ cancelled
//
// The UNIQUE partial index `idx_jobs_one_inflight_per_machine` is the
// per-machine mutex: only one row with status ∈ {pending, running} can exist
// for a given machine_uuid. The DB rejects double-Create with SQLITE_CONSTRAINT;
// the store maps that to ErrInFlight.
//
// retry_of_job_id is the chain pointer for manual retries (failed → new pending).
// Auto-retry is intentionally not implemented (plan B6): dirty disks get worse
// the more we re-write them blindly.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS jobs (
    id              TEXT PRIMARY KEY,
    machine_uuid    TEXT NOT NULL REFERENCES machines(uuid) ON DELETE CASCADE,
    type            TEXT NOT NULL CHECK(type IN ('install','reinstall')),
    image_id        TEXT NOT NULL REFERENCES images(id),
    profile_id      TEXT NOT NULL REFERENCES profiles(id),
    status          TEXT NOT NULL CHECK(status IN ('pending','running','succeeded','failed','cancelled')),
    stage           TEXT NOT NULL DEFAULT '',
    error           TEXT,
    created_at      INTEGER NOT NULL,
    started_at      INTEGER,
    finished_at     INTEGER,
    created_by      TEXT NOT NULL,
    retry_of_job_id TEXT REFERENCES jobs(id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_one_inflight_per_machine
    ON jobs(machine_uuid) WHERE status IN ('pending','running');
CREATE INDEX IF NOT EXISTS idx_jobs_machine    ON jobs(machine_uuid);
CREATE INDEX IF NOT EXISTS idx_jobs_status     ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_created_at ON jobs(created_at);

CREATE TABLE IF NOT EXISTS job_logs (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id   TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    ts       INTEGER NOT NULL,
    level    TEXT NOT NULL CHECK(level IN ('debug','info','warn','error')),
    message  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_job_logs_stream ON job_logs(job_id, id);
`
