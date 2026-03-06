---
name: prends-note
description: >-
  Persiste une information qui doit survivre au compactage ou à la prochaine
  session. Invoque dès que tu identifies quelque chose de non-trivial :
  décision, contrainte, pattern écarté, entité clé du domaine, bloquant.
  N'attends pas qu'on te le demande. L'utilisateur peut aussi invoquer
  via /note ou en langage naturel.
---

# Skill : prends-note

## Principe

Tu es acteur de ta propre mémoire. Quand tu identifies une information
non-triviale qui devrait survivre au compactage ou à la prochaine session,
inscris-la immédiatement dans la DB SQLite locale (`$PROJECT_DIR/.claude/vault.db`).

N'attends pas qu'on te le demande.

## Avant d'inscrire — toujours

Vérifie les types et clés existants pour réutiliser ce qui existe :

```sql
SELECT DISTINCT type, COUNT(*) as n
FROM entities
WHERE namespace = '<namespace>'
GROUP BY type ORDER BY n DESC;

SELECT DISTINCT json_each.key, COUNT(*) as n
FROM entities, json_each(entities.meta)
GROUP BY json_each.key ORDER BY n DESC LIMIT 20;
```

→ Réutilise ce qui existe. Crée un nouveau type seulement si rien d'équivalent n'existe.

## Inscris

```sql
INSERT INTO entities
  (namespace, type, label, sensitivity, ts_created, ts_updated, session_origin, meta)
VALUES
  (?, ?, ?, 0, unixepoch(), unixepoch(), ?,
   jsonb('{"blob_plus":"...","blob_minus":"..."}'));
```

### Mise à jour d'une entité existante

```sql
UPDATE entities
SET meta = jsonb_set(meta, '$.blob_plus', '"nouvelle valeur"'),
    ts_updated = unixepoch()
WHERE id = ?;
```

### Types courants (non exhaustifs, l'usage étend)

| type       | usage                                        |
|------------|----------------------------------------------|
| function   | signature, comportement, edge cases          |
| type       | struct/interface, champs clés, invariants    |
| package    | rôle, dépendances in/out                     |
| file       | rôle, ownership, précautions                 |
| decision   | retenu vs écarté, pourquoi                   |
| constraint | invariant du projet (no CGO, KISS, etc.)     |
| todo       | à faire, bloquant, deadline                  |
| api        | endpoint, auth, gotchas                      |
| dependency | version, choix, alternative écartée          |
| pattern    | retenu vs anti-pattern                       |
| credential | référence UNIQUEMENT — jamais la valeur      |

### Clés meta courantes

- `blob_plus` : ce qu'il faut savoir (le positif, le retenu)
- `blob_minus` : ce qu'il faut éviter (l'écarté, les pièges)
- `target_file` : fichier concerné
- `status` : open | in_progress | blocked | done
- `priority` : high | normal | low
- `deadline` : date ISO
- `url` : lien externe
- `signature` : signature de fonction

## Quand inscrire

- Une décision est prise
- Une contrainte est identifiée
- Un pattern est écarté (`blob_minus` est aussi important que `blob_plus`)
- Un fichier devient central à la session
- Un bloquant émerge
- L'utilisateur dit `/note` ou demande de retenir quelque chose

## Quand NE PAS inscrire

- Contenu brut de fichier
- Output de commande
- Échange exploratoire sans conclusion
- Ce que l'utilisateur retrouve facilement lui-même

## Sécurité

**Règle absolue :** `type=credential` n'inscrit jamais la valeur.
Uniquement : existence, localisation, contexte d'usage.

```sql
-- Correct
INSERT INTO entities (namespace, type, label, sensitivity, ts_created, ts_updated, meta)
VALUES ('project', 'credential', 'ANTHROPIC_API_KEY', 2, unixepoch(), unixepoch(),
        jsonb('{"blob_plus":"présent dans .env","target_file":".env"}'));

-- Jamais
jsonb('{"value":"sk-ant-..."}')
```

## Confirmation

Après chaque inscription, confirme en une ligne ce qui a été inscrit.
Exemple : `Inscrit : decision "no CGO — modernc uniquement" dans context-vault`
