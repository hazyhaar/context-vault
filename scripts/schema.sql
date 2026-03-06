-- context-vault schema
-- Une DB par projet : ${PROJECT_ROOT}/.claude/vault.db

-- Entités : identité uniquement
CREATE TABLE IF NOT EXISTS entities (
    id             INTEGER PRIMARY KEY,
    namespace      TEXT    NOT NULL,
    type           TEXT    NOT NULL,
    label          TEXT    NOT NULL,
    sensitivity    INTEGER DEFAULT 0,  -- 0=public 1=interne 2=secret
    ts_created     INTEGER NOT NULL,
    ts_updated     INTEGER NOT NULL,
    session_origin TEXT
);

-- Attributs : colonnes dynamiques
CREATE TABLE IF NOT EXISTS attributes (
    entity_id  INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    key        TEXT    NOT NULL,
    value      TEXT,
    PRIMARY KEY (entity_id, key)
);

-- Relations entre entités
CREATE TABLE IF NOT EXISTS relations (
    id         INTEGER PRIMARY KEY,
    from_id    INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_id      INTEGER NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    type       TEXT    NOT NULL,       -- depends_on | blocks | subtask_of
    ts_created INTEGER NOT NULL
);

-- Historique des compactages
CREATE TABLE IF NOT EXISTS compact_log (
    id          INTEGER PRIMARY KEY,
    ts          INTEGER NOT NULL,
    session_id  TEXT    NOT NULL,
    reasoning   TEXT,
    query_used  TEXT,
    result_text TEXT
);

-- Sessions
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    started_at INTEGER NOT NULL,
    ended_at   INTEGER,
    project    TEXT
);

-- Vue pratique
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

-- Index
CREATE INDEX IF NOT EXISTS idx_entities_namespace_type ON entities(namespace, type);
CREATE INDEX IF NOT EXISTS idx_entities_ts             ON entities(ts_updated DESC);
CREATE INDEX IF NOT EXISTS idx_attributes_entity       ON attributes(entity_id);
CREATE INDEX IF NOT EXISTS idx_relations_from          ON relations(from_id);
CREATE INDEX IF NOT EXISTS idx_relations_to            ON relations(to_id);
