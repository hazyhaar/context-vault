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

## Étape 3 — SELECT ciblé

Requête ciblée sur l'état actuel. **Pas un SELECT * générique.**

Les entités `sensitivity=2` sont toujours exclues.

```sql
SELECT label,
       meta->>'$.blob_plus'  AS retenu,
       meta->>'$.blob_minus' AS ecarté,
       type
FROM entities
WHERE namespace = ?
  AND sensitivity < 2
  AND (
    type IN ('decision', 'constraint', 'todo')
    OR (type = 'function' AND label LIKE '%<zone chaude>%')
  )
ORDER BY ts_updated DESC
LIMIT 25;
```

### Lire le buffer récent (payloads hooks)

```sql
-- Dernier prompt utilisateur
SELECT payload->>'$.prompt'
FROM buffer WHERE hook = 'user_prompt'
ORDER BY ts DESC LIMIT 1;

-- Dernière réponse assistant
SELECT payload->>'$.last_assistant_message'
FROM buffer WHERE hook = 'stop'
ORDER BY ts DESC LIMIT 1;

-- Erreurs d'outils récentes
SELECT ts, payload->>'$.tool_name' AS outil, payload->>'$.error' AS erreur
FROM buffer WHERE hook = 'post_tool_failure'
ORDER BY ts DESC LIMIT 10;
```

## Étape 4 — Logger

```sql
INSERT INTO compact_log (ts, session_id, trigger, reasoning, query_used, result_text)
VALUES (unixepoch(), ?, 'auto', '<step 1 reasoning>', '<query>', '<résultat>');
```

## Invariants

- Commence **toujours** par le raisonnement, **jamais** par un SELECT
- `sensitivity=2` n'entre **jamais** dans un SELECT
- Log chaque compactage dans `compact_log`
- PreCompact stdout n'est PAS injecté — cette skill est invoquée manuellement avant `/compact`
