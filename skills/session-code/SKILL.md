---
name: session-code
description: >-
  Orchestration session dev : pick todo vault, decomposer en steps,
  coder, tester, archvet, deploy, smoke, step_done, next.
  Invoquer en debut de session d'implementation.
---

# Skill : session-code

## Principe

Orchestration structuree d'une session d'implementation via vault todos et steps.
Chaque action (code, test, lint, deploy, smoke) est une step trackee.
L'auto-transition ferme le todo quand toutes les steps required sont done.

## Workflow

### 1. Orientation

```
vault_list_todos({ "namespace": "<projet>" })
```

Identifier le todo prioritaire non bloque. Le passer en `in_progress`.

### 2. Decomposition en steps

Analyser le todo (label + blob_plus) et creer les steps :

```
vault_add_steps({
  "todo_id": <id>,
  "steps": ["code", "test", "lint", "build", "deploy", "smoke"]
})
```

Steps typiques selon le type de tache :

| Type | Steps |
|------|-------|
| Feature code | code, test, lint, build |
| Feature deployable | code, test, lint, build, deploy, smoke |
| Bug fix | repro-test, fix, test, lint |
| Refactoring | code, test, lint |
| Schema migration | schema, code, test, build, deploy, migrate |

### 3. Boucle d'execution

Pour chaque step, dans l'ordre :

1. **Executer** l'action (ecrire du code, lancer les tests, etc.)
2. **Valider** le resultat (build clean, tests verts, lint ok)
3. **Marquer done** :

```
vault_step_done({ "todo_id": <id>, "step": "code" })
```

4. **Passer a la step suivante**

Si une step echoue, corriger et re-tenter. Ne pas marquer done tant que ce n'est pas valide.

### 4. Auto-transition

Quand toutes les steps required sont done, `vault_step_done` fait automatiquement
passer le todo a `done`. Pas besoin d'appeler `vault_todo_transition` manuellement.

### 5. Todo suivant

Apres completion, revenir a l'etape 1 : `vault_list_todos` pour le prochain todo.

## Regles

- **Pas de code sans todo.** Creer le todo d'abord, meme pour un fix rapide.
- **Pas de step sans validation.** Test rouge d'abord, fix ensuite.
- **Steps granulaires.** Mieux vaut 6 petites steps qu'une grosse.
- **Ne pas sauter deploy/smoke** si le todo concerne un service deploye.
- **archvet** (si disponible) : lancer entre lint et build pour validation architecturale.

## Exemple complet

```
-- 1. Pick todo
vault_list_todos({ "namespace": "context-vault" })
-- -> #523 [high] Table todo_steps...

-- 2. Start
vault_todo_transition({ "id": 523, "status": "in_progress" })

-- 3. Add steps
vault_add_steps({ "todo_id": 523, "steps": ["schema", "handlers", "dispatch", "build"] })

-- 4. Execute
-- ... ecrire le code ...
vault_step_done({ "todo_id": 523, "step": "schema" })
-- ... ecrire les handlers ...
vault_step_done({ "todo_id": 523, "step": "handlers" })
-- ... ajouter au dispatch ...
vault_step_done({ "todo_id": 523, "step": "dispatch" })
-- ... GOWORK=off CGO_ENABLED=0 go build ...
vault_step_done({ "todo_id": 523, "step": "build" })
-- -> "todo 523 auto-transitioned to done"

-- 5. Next todo
vault_list_todos({ "namespace": "context-vault" })
```
