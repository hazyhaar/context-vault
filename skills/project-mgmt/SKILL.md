---
name: project-mgmt
description: >-
  Gestion de todos structurés avec dépendances et délais. Invoquer dès que
  plus de deux tâches émergent, ou quand des dépendances mutuelles
  apparaissent. Les todos persistent à travers les compactages et sessions.
---

# Skill : project-mgmt

## Principe

Gestion de todos structurés dans la DB SQLite (`$PROJECT_DIR/.claude/vault.db`).
Les todos persistent à travers les compactages et les sessions.

Invoquer dès que plus de deux tâches émergent, ou quand des dépendances
mutuelles apparaissent.

## Créer un todo

```sql
INSERT INTO entities
  (namespace, type, label, sensitivity, ts_created, ts_updated, session_origin, meta)
VALUES
  (?, 'todo', ?, 0, unixepoch(), unixepoch(), ?,
   jsonb('{"blob_plus":"ce qu il faut faire","status":"open","priority":"normal"}'));
```

## Dépendance

```sql
-- "A depends_on B" = A ne peut pas commencer avant B terminé
INSERT INTO relations (from_id, to_id, type, ts_created)
VALUES (<A_id>, <B_id>, 'depends_on', unixepoch());
```

## Plan d'exécution

```sql
SELECT
    e.label,
    e.meta->>'$.status'    AS status,
    e.meta->>'$.priority'  AS priority,
    e.meta->>'$.deadline'  AS deadline,
    e.meta->>'$.blob_plus' AS quoi,
    e.meta->>'$.blob_minus' AS pieges,
    GROUP_CONCAT(dep.label, ' | ') AS attend
FROM entities e
LEFT JOIN relations r   ON r.from_id = e.id AND r.type = 'depends_on'
LEFT JOIN entities dep  ON dep.id = r.to_id
WHERE e.namespace = ?
  AND e.type = 'todo'
  AND e.meta->>'$.status' != 'done'
GROUP BY e.id
ORDER BY
    CASE e.meta->>'$.priority' WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END,
    e.meta->>'$.deadline' ASC NULLS LAST;
```

## Terminer

```sql
UPDATE entities
SET meta = jsonb_set(meta, '$.status', '"done"'),
    ts_updated = unixepoch()
WHERE id = ?;
```

## Autres transitions

```sql
-- En cours
UPDATE entities
SET meta = jsonb_set(meta, '$.status', '"in_progress"'),
    ts_updated = unixepoch()
WHERE id = ?;

-- Bloqué
UPDATE entities
SET meta = jsonb_set(meta, '$.status', '"blocked"'),
    ts_updated = unixepoch()
WHERE id = ?;
```
