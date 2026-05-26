package images

// SQLite schema for image catalog and in-flight chunked uploads. Layered onto
// the same connection as inventory; FKs work across tables.
//
// Two tables:
//   images           — finalized images, indexed by content hash (sha256)
//   upload_sessions  — in-flight chunked uploads; rows survive restart so a
//                      partially-uploaded image can be resumed or GC'd.
//
// chunk-level state lives on disk (one file per chunk under .tmp/{upload_id}/),
// not in the DB. We only persist enough metadata to reconstruct progress and
// to garbage-collect stale sessions.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS images (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    version       TEXT,
    family        TEXT,
    format        TEXT NOT NULL,
    size_bytes    INTEGER NOT NULL,
    virtual_size  INTEGER,
    sha256        TEXT NOT NULL UNIQUE,
    uploaded_at   INTEGER NOT NULL,
    uploaded_by   TEXT NOT NULL,
    last_used_at  INTEGER,
    notes         TEXT,
    metadata_json TEXT
);
CREATE INDEX IF NOT EXISTS idx_images_uploaded_at ON images(uploaded_at DESC);

CREATE TABLE IF NOT EXISTS upload_sessions (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    version         TEXT,
    family          TEXT,
    notes           TEXT,
    expected_sha256 TEXT NOT NULL,
    total_size      INTEGER NOT NULL,
    chunk_size      INTEGER NOT NULL,
    num_chunks      INTEGER NOT NULL,
    uploaded_chunks INTEGER NOT NULL DEFAULT 0,
    uploaded_by     TEXT NOT NULL,
    started_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_upload_sessions_started_at ON upload_sessions(started_at);
`
