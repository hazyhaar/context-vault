---
name: hot-contexte
description: >-
  À invoquer quand le compactage est imminent. Raisonne sur ce que le
  prochain Claude devra savoir pour continuer sans friction, complète
  la DB si nécessaire, puis formule le SELECT qui extrait exactement ça.
---

# Skill : hot-contexte

## Principe

Le compactage est imminent. Tu dois préparer le contexte pour le prochain
Claude en raisonnant explicitement sur ce qui est important, en complétant
la DB si nécessaire, puis en formulant un SELECT ciblé.

La DB est à `$PROJECT_DIR/.claude/vault.db`.

## Étape 1 — Raisonnement explicite (ne pas sauter)

Réponds à ces questions avant d'écrire la moindre requête :

1. **Objectif en cours** — Quel est l'objectif ? Où en est-on exactement ?
2. **Bloquants** — Qu'est-ce qui est bloquant ou fragile en ce moment ?
3. **Décisions récentes** — Quelles décisions le prochain Claude ignorerait sans la DB ?
4. **Fichiers chauds** — Quels fichiers sont modifiés ou sur le point de l'être ?
5. **Contraintes implicites** — Y a-t-il des contraintes apprises en session et pas encore inscrites ?

## Étape 2 — Compléter si nécessaire

Si le raisonnement révèle des trous → invoquer prends-note avant de continuer.

## Étape 3 — Construire le SELECT

Requête ciblée sur l'état actuel. **Pas un SELECT * générique.**

Les entités `sensitivity=2` sont toujours exclues.

Exemple (session de débogage hook) :

```sql
SELECT e.type, e.label, a_plus.value AS blob_plus, a_minus.value AS blob_minus
FROM entities e
LEFT JOIN attributes a_plus  ON a_plus.entity_id  = e.id AND a_plus.key  = 'blob_plus'
LEFT JOIN attributes a_minus ON a_minus.entity_id = e.id AND a_minus.key = 'blob_minus'
WHERE e.namespace = 'context-vault'
  AND e.sensitivity < 2
  AND (
    e.type IN ('decision', 'constraint', 'todo')
    OR (e.type = 'function' AND e.label LIKE '%Compact%')
  )
ORDER BY e.ts_updated DESC;
```

## Étape 4 — Logger

```sql
INSERT INTO compact_log (ts, session_id, reasoning, query_used, result_text)
VALUES (unixepoch(), ?, '<step 1 reasoning>', '<query>', '<résultat>');
```

## Invariants

- Commence **toujours** par le raisonnement, **jamais** par un SELECT
- `sensitivity=2` n'entre **jamais** dans un SELECT
- Log chaque compactage dans `compact_log`
