// CLAUDE:SUMMARY Vault SQLite schema — DDL + migrations for vault.db.
// CLAUDE:DEPENDS (none)
// CLAUDE:EXPORTS Schema, Migrate
package vault

import "database/sql"

// Schema is the full DDL for vault.db.
const Schema = `CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT    PRIMARY KEY,
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER,
    project     TEXT,
    model       TEXT
);

CREATE TABLE IF NOT EXISTS entities (
    id             INTEGER PRIMARY KEY,
    namespace      TEXT    NOT NULL,
    type           TEXT    NOT NULL,
    label          TEXT    NOT NULL,
    sensitivity    INTEGER DEFAULT 0,
    ts_created     INTEGER NOT NULL,
    ts_updated     INTEGER NOT NULL,
    session_origin TEXT,
    meta           BLOB
);

CREATE TABLE IF NOT EXISTS relations (
    id         INTEGER PRIMARY KEY,
    from_id    INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_id      INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    type       TEXT    NOT NULL,
    ts_created INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS buffer (
    id         INTEGER PRIMARY KEY,
    session_id TEXT    NOT NULL,
    ts         INTEGER NOT NULL,
    hook       TEXT    NOT NULL,
    payload    BLOB    NOT NULL,
    size_est   INTEGER NOT NULL,
    processed  INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS todo_steps (
    todo_id  INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    step     TEXT    NOT NULL,
    step_key TEXT    NOT NULL,
    required INTEGER DEFAULT 1,
    done     INTEGER DEFAULT 0,
    done_at  INTEGER,
    PRIMARY KEY (todo_id, step_key)
);

CREATE TABLE IF NOT EXISTS compact_log (
    id          INTEGER PRIMARY KEY,
    ts          INTEGER NOT NULL,
    session_id  TEXT    NOT NULL,
    trigger     TEXT,
    reasoning   TEXT,
    query_used  TEXT,
    result_text TEXT
);

CREATE TABLE IF NOT EXISTS agents (
    session_id    TEXT PRIMARY KEY,
    role          TEXT NOT NULL DEFAULT 'worker',
    connected     INTEGER NOT NULL DEFAULT 0,
    registered_at INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_entities_ns_type ON entities(namespace, type);
CREATE INDEX IF NOT EXISTS idx_entities_ts      ON entities(ts_updated DESC);
CREATE INDEX IF NOT EXISTS idx_relations_from   ON relations(from_id);
CREATE INDEX IF NOT EXISTS idx_relations_to     ON relations(to_id);
CREATE INDEX IF NOT EXISTS idx_buffer_session   ON buffer(session_id, processed, ts);
`

// Migrate applies forward-only migrations to an existing vault.db.
func Migrate(db *sql.DB) {
	// v2026-03-21: add step_key column for idempotent step dedup.
	db.Exec(`ALTER TABLE todo_steps ADD COLUMN step_key TEXT NOT NULL DEFAULT ''`)
	db.Exec(`UPDATE todo_steps SET step_key = CASE
		WHEN instr(step, ':') > 0 THEN trim(substr(step, 1, instr(step, ':') - 1))
		ELSE step END
		WHERE step_key = ''`)
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_todo_steps_key ON todo_steps(todo_id, step_key)`)

	// v2026-03-21b: add agents table for persistent session tracking.
	db.Exec(`CREATE TABLE IF NOT EXISTS agents (
		session_id    TEXT PRIMARY KEY,
		role          TEXT NOT NULL DEFAULT 'worker',
		connected     INTEGER NOT NULL DEFAULT 0,
		registered_at INTEGER NOT NULL,
		last_seen_at  INTEGER NOT NULL
	)`)
}
