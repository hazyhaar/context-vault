# context-vault — Usage des outils MCP

7 outils disponibles via le serveur MCP stdio. Tous opèrent sur `$PROJECT_DIR/.claude/vault.db`.

## Concepts

- **namespace** : regroupe les entités par projet/service (ex: `repvow`, `horum`, `infra`)
- **type** : `todo`, `decision`, `constraint`, `pattern`, `function`, `type`, `package`, `file`, `api`, `dependency`, `credential`
- **meta** : blob JSON libre. Clés conventionnelles : `status`, `priority`, `blob_plus`, `blob_minus`, `target_file`, `deadline`
- **sensitivity** : `0` public, `1` internal, `2` secret (exclu des résultats sauf delete)

## vault_upsert_entity

Crée ou met à jour une entité. Omit `id` pour créer, fournis `id` pour update.

```
# Inscrire une décision
vault_upsert_entity({
  "namespace": "repvow",
  "type": "decision",
  "label": "SQLite WAL obligatoire",
  "meta": {"blob_plus": "Performance et fiabilité", "priority": "high"}
})
→ created entity 42

# Inscrire un todo
vault_upsert_entity({
  "namespace": "horum",
  "type": "todo",
  "label": "Ajouter goleak_test.go au package mcpquic",
  "meta": {"status": "open", "priority": "high", "target_file": "horum/internal/mcpquic/"}
})
→ created entity 43

# Modifier un todo existant
vault_upsert_entity({
  "id": 43,
  "label": "Ajouter goleak_test.go au package mcpquic",
  "meta": {"status": "in_progress", "priority": "high"}
})
→ updated entity 43

# Inscrire une contrainte
vault_upsert_entity({
  "namespace": "global",
  "type": "constraint",
  "label": "CGO_ENABLED=0 — jamais de dépendance C"
})
→ created entity 44

# Inscrire un credential (jamais la valeur, uniquement l'existence)
vault_upsert_entity({
  "namespace": "infra",
  "type": "credential",
  "label": "ANTHROPIC_API_KEY",
  "sensitivity": 2,
  "meta": {"blob_plus": "présent dans .env sur VPS BO", "target_file": ".env"}
})
→ created entity 45
```

## vault_get_context

Vue synthétique des todos/decisions/constraints actifs. Idéal en début de session ou après compaction.

```
vault_get_context({})
→ ## Todos actifs
  - [high] Ajouter goleak_test.go au package mcpquic
  ## Decisions
  - [high] SQLite WAL obligatoire — Performance et fiabilité
  ## Contraintes
  - CGO_ENABLED=0 — jamais de dépendance C

# Filtré par namespace
vault_get_context({"namespace": "repvow"})
```

## vault_search_entities

Recherche par type, namespace, ou substring sur le label.

```
# Chercher tout ce qui contient "SQLite"
vault_search_entities({"query": "SQLite"})

# Lister toutes les decisions du namespace horum
vault_search_entities({"type": "decision", "namespace": "horum"})

# Limiter les résultats
vault_search_entities({"query": "test", "limit": 5})
```

## vault_list_todos

Liste les todos avec statut, priorité, deadline et dépendances.
Les blockers affichent les IDs des todos dont dépend chaque tâche (`depends_on`).

```
vault_list_todos({})
→ #43 Ajouter goleak_test.go au package mcpquic [high] — in_progress blocked by: 42
     Besoin du refactor chi/v5 d'abord
  #42 Migrer handlers vers chi/v5 [high] — open (deadline: 2026-03-15)

# Inclure les todos terminés
vault_list_todos({"include_done": true})

# Filtrer par namespace
vault_list_todos({"namespace": "horum"})
```

## vault_todo_transition

Change le statut d'un todo. Statuts : `open`, `in_progress`, `blocked`, `done`.

```
vault_todo_transition({"id": 43, "status": "done"})
→ todo 43 -> done

vault_todo_transition({"id": 43, "status": "blocked"})
→ todo 43 -> blocked
```

## vault_create_relation

Lie deux entités. Types : `depends_on`, `blocks`, `subtask_of`.

```
# Le todo 43 dépend de la decision 42
vault_create_relation({"from_id": 43, "to_id": 42, "type": "depends_on"})
→ relation depends_on: 43 -> 42

# Le todo 46 est une sous-tâche du todo 43
vault_create_relation({"from_id": 46, "to_id": 43, "type": "subtask_of"})
```

## vault_delete_entity

Supprime une entité et ses relations (CASCADE sur from_id et to_id).

```
vault_delete_entity({"id": 45})
→ deleted entity 45
```

## Patterns d'usage courants

**Début de tâche** : `vault_get_context` pour voir l'état du projet, puis `vault_list_todos` pour le backlog.

**Pendant le travail** : `vault_upsert_entity` pour inscrire les décisions prises et les pièges découverts au fil de l'eau.

**Fin de tâche** : `vault_todo_transition` pour marquer les todos done, créer les nouveaux todos pour la suite.

**Après compaction** : `vault_get_context` est automatiquement injecté par le hook — les décisions et todos survivent à la perte de contexte.
