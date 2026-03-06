#!/usr/bin/env python3
"""context-vault hooks — SessionStart & SessionEnd.

Minimal hook binary. The LLM decides what to write via skills;
this only handles session lifecycle and DB setup.
"""

import json
import os
import sqlite3
import stat
import sys

SCHEMA = """
CREATE TABLE IF NOT EXISTS entities (
    id             INTEGER PRIMARY KEY,
    namespace      TEXT    NOT NULL,
    type           TEXT    NOT NULL,
    label          TEXT    NOT NULL,
    sensitivity    INTEGER DEFAULT 0,
    ts_created     INTEGER NOT NULL,
    ts_updated     INTEGER NOT NULL,
    session_origin TEXT
);

CREATE TABLE IF NOT EXISTS attributes (
    entity_id  INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    key        TEXT    NOT NULL,
    value      TEXT,
    PRIMARY KEY (entity_id, key)
);

CREATE TABLE IF NOT EXISTS relations (
    id         INTEGER PRIMARY KEY,
    from_id    INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_id      INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    type       TEXT    NOT NULL,
    ts_created INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS compact_log (
    id          INTEGER PRIMARY KEY,
    ts          INTEGER NOT NULL,
    session_id  TEXT    NOT NULL,
    reasoning   TEXT,
    query_used  TEXT,
    result_text TEXT
);

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    started_at INTEGER NOT NULL,
    ended_at   INTEGER,
    project    TEXT
);

CREATE VIEW IF NOT EXISTS entity_view AS
SELECT
    e.id, e.namespace, e.type, e.label, e.sensitivity,
    e.ts_updated, e.session_origin,
    MAX(CASE WHEN a.key = 'blob_plus'   THEN a.value END) AS blob_plus,
    MAX(CASE WHEN a.key = 'blob_minus'  THEN a.value END) AS blob_minus,
    MAX(CASE WHEN a.key = 'target_file' THEN a.value END) AS target_file,
    MAX(CASE WHEN a.key = 'status'      THEN a.value END) AS status,
    MAX(CASE WHEN a.key = 'deadline'    THEN a.value END) AS deadline,
    MAX(CASE WHEN a.key = 'priority'    THEN a.value END) AS priority
FROM entities e
LEFT JOIN attributes a ON a.entity_id = e.id
GROUP BY e.id;

CREATE INDEX IF NOT EXISTS idx_entities_namespace_type ON entities(namespace, type);
CREATE INDEX IF NOT EXISTS idx_entities_ts             ON entities(ts_updated DESC);
CREATE INDEX IF NOT EXISTS idx_attributes_entity       ON attributes(entity_id);
CREATE INDEX IF NOT EXISTS idx_relations_from          ON relations(from_id);
CREATE INDEX IF NOT EXISTS idx_relations_to            ON relations(to_id);
"""


def get_db_path(project_dir: str) -> str:
    return os.path.join(project_dir, ".claude", "vault.db")


def ensure_gitignore(project_dir: str) -> None:
    gitignore = os.path.join(project_dir, ".gitignore")
    entry = ".claude/vault.db"

    lines = []
    if os.path.exists(gitignore):
        with open(gitignore, "r") as f:
            lines = f.read().splitlines()

    if entry in [l.strip() for l in lines]:
        return

    with open(gitignore, "a") as f:
        if lines and lines[-1] != "":
            f.write("\n")
        f.write(entry + "\n")


def session_start(db_path: str, session_id: str, project_dir: str) -> None:
    # Ensure .claude directory exists
    os.makedirs(os.path.dirname(db_path), exist_ok=True)

    conn = sqlite3.connect(db_path)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    conn.executescript(SCHEMA)

    if session_id:
        conn.execute(
            "INSERT OR IGNORE INTO sessions (id, started_at, project) "
            "VALUES (?, unixepoch(), ?)",
            (session_id, project_dir),
        )
    conn.commit()
    conn.close()

    # chmod 600
    os.chmod(db_path, stat.S_IRUSR | stat.S_IWUSR)
    for suffix in ("-wal", "-shm"):
        p = db_path + suffix
        if os.path.exists(p):
            os.chmod(p, stat.S_IRUSR | stat.S_IWUSR)

    # .gitignore
    try:
        ensure_gitignore(project_dir)
    except Exception as e:
        print(f"warning: gitignore: {e}", file=sys.stderr)


def session_end(db_path: str, session_id: str) -> None:
    if not session_id or not os.path.exists(db_path):
        return

    conn = sqlite3.connect(db_path)
    conn.execute(
        "UPDATE sessions SET ended_at = unixepoch() WHERE id = ?",
        (session_id,),
    )
    conn.commit()
    conn.close()


def main() -> None:
    if len(sys.argv) < 2:
        print("Usage: context-vault <hook>", file=sys.stderr)
        print("Hooks: SessionStart, SessionEnd", file=sys.stderr)
        sys.exit(1)

    hook = sys.argv[1]

    # Read hook input from stdin (JSON)
    session_id = ""
    project_dir = ""
    try:
        data = json.load(sys.stdin)
        session_id = data.get("session_id", "")
        project_dir = data.get("project_dir", "")
    except Exception:
        session_id = os.environ.get("CLAUDE_SESSION_ID", "")
        project_dir = os.environ.get("CLAUDE_PROJECT_DIR", "")

    if not project_dir:
        project_dir = os.getcwd()

    db_path = get_db_path(project_dir)

    if hook == "SessionStart":
        session_start(db_path, session_id, project_dir)
    elif hook == "SessionEnd":
        session_end(db_path, session_id)
    else:
        print(f"Unknown hook: {hook}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
