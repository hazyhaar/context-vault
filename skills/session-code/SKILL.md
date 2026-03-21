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

### 0. Contexte vault

```bash
# Lire le session_id stable (UUID du fichier JSONL de la conversation)
ls -t ~/.claude/projects/-devhoros/*.jsonl | head -1
# Extraire l'UUID du nom de fichier (ex: 6df72794-033f-4b9a-8007-fadbe6d4fad1)
```

```
vault_assume_role({ role: "worker", session_id: "<uuid>" })
vault_get_context({})
vault_list_todos({})
```

Le session_id est l'UUID extrait du nom de fichier JSONL. Il survit aux /mcp reconnects
car il est lie a la conversation, pas au process. Le daemon enregistre le worker dans
la table agents avec cet UUID stable.

Les checkpoints que tu postes seront pushes en temps reel vers le superviseur.
**Tu DOIS poster des checkpoints** (voir §5).

### 1. Briefing

Chercher une mission vault qui cadre la session :

```
vault_search_entities({ "type": "mission" })
```

Si une mission active existe pour le namespace courant : la lire.
Elle definit le perimetre exact (numeros de todos, contexte technique, contraintes).
**Le perimetre de la mission est ta frontiere. Tu ne travailles QUE sur les todos listees.**

Si aucune mission : passer a l'etape 2 normalement.

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

### Outils Go obligatoires (fichiers .go uniquement)

Les outils LSP gopls et archaix sont disponibles via MCP. Les utiliser systematiquement.

#### Pendant la step "code"

- **Avant de lire un fichier Go** : `go_file_context` apres le premier Read partiel — montre les dependances effectives du fichier
- **Avant de modifier un symbole public** : `go_symbol_references` — mesure l'impact cross-module. Ne pas modifier un symbole public sans connaitre ses appelants
- **Recherche cross-module** : `go_search` quand un symbole est introuvable par Grep
- **API d'un package** : `go_package_api` pour comprendre l'API publique avant d'importer

#### Pendant la step "lint" (OBLIGATOIRE pour tout edit Go)

- **`go_diagnostics`** sur CHAQUE fichier .go modifie — pas optionnel. Ne pas passer a la step suivante tant que go_diagnostics retourne des erreurs
- **`archvet ./...`** si disponible — validation architecturale (annotations, import bans, patterns)
- Corriger les erreurs diagnostics AVANT de marquer la step done

#### Pendant la step "build"

- **`go_diagnostics`** une derniere fois post-build si le build echoue — les messages d'erreur de go_diagnostics sont plus precis que ceux de go build

#### Resume

| Moment | Outil | Obligatoire |
|--------|-------|-------------|
| Apres premier Read .go | `go_file_context` | oui |
| Avant modif symbole public | `go_symbol_references` | oui |
| Symbole introuvable | `go_search` | si besoin |
| Comprendre un package | `go_package_api` | si besoin |
| Apres tout edit .go | `go_diagnostics` | **oui** |
| Entre lint et build | `archvet ./...` | si disponible |

### 4. Auto-transition

Quand toutes les steps required sont done, `vault_step_done` fait automatiquement
passer le todo a `done`. Pas besoin d'appeler `vault_todo_transition` manuellement.

### 5. Checkpoint de completion (OBLIGATOIRE)

Quand un todo passe a `done`, poster un checkpoint **meme si la mission n'est pas finie** :

```
vault_upsert_entity({
  type: "checkpoint",
  namespace: "<projet>",
  label: "CHECKPOINT todo #<id> done — <resume>",
  meta: {
    question: "<ce qui a ete fait, fichiers modifies, tests>",
    blocking: false
  }
})
```

`blocking: false` = le superviseur est notifie mais le worker continue sans attendre.
`blocking: true` = reserve aux decisions, deploys, imprevu (le worker ATTEND la reponse).

**Pourquoi :** le superviseur recoit les checkpoints en push via le daemon context-vault.
Sans checkpoint, le superviseur ne sait pas que le todo est done. La session est invisible.

### 6. Todo suivant

Apres completion, revenir a l'etape 2 : `vault_list_todos` pour le prochain todo.

### 7. Cloture de mission (OBLIGATOIRE)

Quand **tous les todos de la mission** sont done, creer un checkpoint blocking
pour rendre compte au superviseur :

```
vault_upsert_entity({
  type: "checkpoint",
  namespace: "<projet>",
  label: "CHECKPOINT mission #<mission_id> complete — <resume 1 ligne>",
  meta: {
    question: "<rapport structure : ce qui a ete livre, fichiers modifies, tests, findings, ce qui reste hors scope>",
    blocking: true,
    options: ["approuver", "refuser", "questions"]
  }
})
```

**ATTENDRE la reponse du superviseur.** Ne pas terminer la session, ne pas passer
a d'autres todos hors mission. Le superviseur valide, pose des questions, ou demande
des corrections. Reprendre uniquement apres `meta.answer`.

Si aucune mission n'encadre la session (mode libre), cette etape est optionnelle.

## Conventions de nommage vault (OBLIGATOIRE)

Les labels vault utilisent des prefixes structures. Ces prefixes sont parses par claude-vault pour generer des rapports agreges. **Ne pas inventer de prefixes.**

### Labels todo

| Prefixe | Usage | Exemple |
|---------|-------|---------|
| FIX | Correction de bug | FIX WAL checkpoint starvation |
| FEAT | Nouvelle fonctionnalite | FEAT pulse.db vues materialisees |
| CLEANUP | Nettoyage infra/code | CLEANUP supprimer doublons |
| REFAC | Refactoring | REFAC extracteur metadata |
| DOC | Documentation | DOC CLAUDE.md tracqlite |
| TEST | Tests | TEST couverture redaction |

### Labels checkpoint

| Prefixe | Usage | Exemple |
|---------|-------|---------|
| DEPLOY | Demande approbation deploy | DEPLOY : sqltop timer sur KS-5-B |
| IMPREVU | Evenement hors perimetre | IMPREVU : todos introuvables |
| CHANGEMENT DE PLAN | Modification du scope | Changement de plan : abandon P-PM-3 |
| CHECKPOINT | Point d'etape standard | CHECKPOINT test exhaustif 13/13 PASS |
| AUDIT | Rapport d'audit | AUDIT claude-vault rapport complet |

### Labels decision

| Prefixe | Usage | Exemple |
|---------|-------|---------|
| MISSION | Debut de mission, perimetre | MISSION tracqlite 4 plans |
| AUDIT | Rapport d'audit complet | AUDIT claude-vault rapport |

### Regle

Tout label vault DOIT commencer par un de ces prefixes. Un label sans prefixe est un bug de convention.

## Regles

- **Pas de code sans todo.** Creer le todo d'abord, meme pour un fix rapide.
- **Pas de step sans validation.** Test rouge d'abord, fix ensuite.
- **Steps granulaires.** Mieux vaut 6 petites steps qu'une grosse.
- **Ne pas sauter deploy/smoke** si le todo concerne un service deploye.
- **archvet** (si disponible) : lancer entre lint et build pour validation architecturale.

## Perimetre et escalade (NON NEGOCIABLE)

**Ton perimetre = les todos qui te sont assignees. Rien d'autre.**

Tu n'as PAS le droit de :
- Creer de nouvelles todos
- Modifier le label ou le blob_plus des todos existantes
- Ajouter des steps non prevues dans la decomposition initiale
- Modifier des fichiers hors du scope des todos assignees
- Fixer un bug decouvert en chemin (le signaler via checkpoint, pas le fixer)
- Refactorer du code adjacent
- Ajouter des commentaires, docstrings, annotations dans du code que tu ne modifies pas
- Prendre une decision architecturale, meme mineure

### Protocole STOP

Si un imprevu survient qui depasse le perimetre de tes todos :

1. **STOP immediat.** Arreter tout travail en cours.
2. **Checkpoint bloquant :**

```
vault_upsert_entity({
  type: "checkpoint",
  namespace: "<projet>",
  label: "IMPREVU : <description courte>",
  meta: {
    question: "<description precise + pourquoi ca depasse ton perimetre>",
    options: ["approuver l'extension", "ignorer et continuer", "autre directive"],
    blocking: true,
    context: "<ce que tu as observe>"
  }
})
```

3. **ATTENDRE.** Ne JAMAIS continuer sans que `meta.answer` soit rempli par le superviseur.

Cas qui declenchent le protocole STOP :
- Bug decouvert hors perimetre
- Fix qui necessite de modifier un fichier non prevu
- Hesitation entre 2 approches pour une step
- Test qui echoue et dont le fix sort du scope
- Le code ne fait pas ce que la todo decrit
- Tout deploiement sur un serveur (systemd enable, restart service)
- Finding inattendu pendant un diagnostic (ex: "aucun service n'a X" alors qu'on pensait que certains l'avaient). Signaler le finding + lister l'impact reel avant d'agir
- Modification de fichiers systeme (unit systemd, cron, /etc/*) touchant plus de 2 services

### Changement de plan

Meme regle pour les changements de plan (nouveau scope, abandon d'une todo,
reordonnancement, changement d'approche) : protocole STOP, checkpoint bloquant, attente.

```
vault_upsert_entity({
  type: "checkpoint",
  namespace: "<projet>",
  label: "Changement de plan : <description>",
  meta: {
    question: "Le plan initial etait X. Je propose Y parce que Z. Approuves ?",
    options: ["approuver", "refuser", "modifier"],
    blocking: true
  }
})
```

Ne JAMAIS modifier le scope, ajouter des todos, ou changer l'approche sans reponse.

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
