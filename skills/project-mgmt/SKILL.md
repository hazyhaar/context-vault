---
name: project-mgmt
description: >-
  Gestion de todos structurés avec dépendances et délais. Invoquer dès que
  plus de deux tâches émergent, ou quand des dépendances mutuelles
  apparaissent. Les todos persistent à travers les compactages et sessions.
---

# Skill : project-mgmt

## Principe

Gestion de todos structurés via les outils MCP context-vault.
Les todos persistent à travers les compactages et les sessions.

Invoquer dès que plus de deux tâches émergent, ou quand des dépendances
mutuelles apparaissent.

## Créer un todo

```
vault_upsert_entity({
  "namespace": "repvow",
  "type": "todo",
  "label": "Migrer les handlers vers chi/v5",
  "meta": {
    "status": "open",
    "priority": "high",
    "blob_plus": "Handlers actuels sur net/http pur",
    "blob_minus": "Ne pas casser les middlewares auth",
    "target_file": "repvow/internal/handlers/",
    "deadline": "2026-03-15"
  }
})
```

## Dépendance

"A depends_on B" = A ne peut pas commencer avant B terminé.

```
vault_create_relation({"from_id": 43, "to_id": 42, "type": "depends_on"})
```

Autres types : `blocks` (inverse de depends_on), `subtask_of`.

## Plan d'exécution

```
vault_list_todos({"namespace": "repvow"})
```

Affiche statut, priorité, deadline et blockers pour chaque todo ouvert.

Pour une vue plus large incluant decisions et contraintes :

```
vault_get_context({"namespace": "repvow"})
```

## Transitions

```
vault_todo_transition({"id": 43, "status": "in_progress"})
vault_todo_transition({"id": 43, "status": "blocked"})
vault_todo_transition({"id": 43, "status": "done"})
```

Statuts valides : `open`, `in_progress`, `blocked`, `done`.

## Séquence type

1. `vault_list_todos` — voir le backlog
2. `vault_todo_transition` → `in_progress` sur la tâche choisie
3. Travailler
4. `vault_upsert_entity` — inscrire les décisions prises et pièges découverts
5. `vault_todo_transition` → `done`
6. `vault_upsert_entity` — créer les todos suivants si nécessaire
