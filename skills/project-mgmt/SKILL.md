---
name: project-mgmt
description: >-
  Gestion de todos structurés avec dépendances et délais. Invoquer dès que
  plus de deux tâches émergent, ou quand des dépendances mutuelles
  apparaissent. Les todos persistent à travers les compactages.
---

# Skill : project-mgmt

## Principe

Gestion de todos structurés dans la DB SQLite (`$PROJECT_DIR/.claude/vault.db`).
Les todos persistent à travers les compactages et les sessions.

Invoquer dès que plus de deux tâches émergent, ou quand des dépendances
mutuelles apparaissent.

## Inscrire un todo

```sql
INSERT INTO entities (namespace, type, label, sensitivity, ts_created, ts_updated, session_origin)
VALUES (?, 'todo', ?, 0, unixepoch(), unixepoch(), ?);

INSERT INTO attributes (entity_id, key, value) VALUES
  (last_insert_rowid(), 'blob_plus',  'ce qu il faut faire'),
  (last_insert_rowid(), 'blob_minus', 'pièges connus'),   -- si applicable
  (last_insert_rowid(), 'status',     'open'),            -- open | in_progress | blocked | done
  (last_insert_rowid(), 'priority',   'normal'),          -- high | normal | low
  (last_insert_rowid(), 'deadline',   '2026-03-15');      -- si applicable
```

## Inscrire une dépendance

```sql
-- "A depends_on B" = A ne peut pas commencer avant B terminé
INSERT INTO relations (from_id, to_id, type, ts_created)
VALUES (<A_id>, <B_id>, 'depends_on', unixepoch());
```

## Vue plan d'exécution

```sql
SELECT
    e.label,
    a_status.value    AS status,
    a_priority.value  AS priority,
    a_deadline.value  AS deadline,
    a_plus.value      AS quoi,
    a_minus.value     AS pieges,
    GROUP_CONCAT(dep.label, ' | ') AS attend
FROM entities e
LEFT JOIN attributes a_status   ON a_status.entity_id   = e.id AND a_status.key   = 'status'
LEFT JOIN attributes a_priority ON a_priority.entity_id = e.id AND a_priority.key = 'priority'
LEFT JOIN attributes a_deadline ON a_deadline.entity_id = e.id AND a_deadline.key = 'deadline'
LEFT JOIN attributes a_plus     ON a_plus.entity_id     = e.id AND a_plus.key     = 'blob_plus'
LEFT JOIN attributes a_minus    ON a_minus.entity_id    = e.id AND a_minus.key    = 'blob_minus'
LEFT JOIN relations r            ON r.from_id = e.id AND r.type = 'depends_on'
LEFT JOIN entities dep           ON dep.id = r.to_id
WHERE e.namespace = ?
  AND e.type = 'todo'
  AND COALESCE(a_status.value, 'open') != 'done'
GROUP BY e.id
ORDER BY
    CASE a_priority.value WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END,
    a_deadline.value ASC NULLS LAST;
```

## Marquer comme terminé

```sql
UPDATE attributes
SET value = 'done'
WHERE entity_id = <id> AND key = 'status';

UPDATE entities SET ts_updated = unixepoch() WHERE id = <id>;
```

## Autres transitions

```sql
-- Marquer en cours
UPDATE attributes SET value = 'in_progress' WHERE entity_id = <id> AND key = 'status';
UPDATE entities SET ts_updated = unixepoch() WHERE id = <id>;

-- Marquer bloqué
UPDATE attributes SET value = 'blocked' WHERE entity_id = <id> AND key = 'status';
UPDATE entities SET ts_updated = unixepoch() WHERE id = <id>;
```
