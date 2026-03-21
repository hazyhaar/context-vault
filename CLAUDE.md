> **Protocole** — Avant toute tâche, lire [`../CLAUDE.md`](../CLAUDE.md) §Protocole de recherche.
> Commandes obligatoires : `Read <dossier>/CLAUDE.md` → `Grep "CLAUDE:SUMMARY"` → `Grep "CLAUDE:WARN" <fichier>`.
> **Interdit** : Bash(grep/cat/find) au lieu de Grep/Read. Ne jamais lire un fichier entier en première intention.

# CLAUDE.md — context-vault

> **Outils MCP** : voir [`USAGE.md`](USAGE.md) pour la référence complète des 7 outils vault avec exemples.

## Responsabilité

Serveur HTTP hooks pour mémoire persistante Claude Code. Reçoit les événements de session via POST localhost:9742, écrit en SQLite JSONB, signale via stdout quand le contexte accumulé dépasse le seuil (RingDumper).

## Dépendances

- `modernc.org/sqlite` — SQLite sans CGO

## Dépendants

- Claude Code (via hooks HTTP configurés dans `.claude/settings.json`)
- Skills : `prends-note`, `hot-contexte`, `project-mgmt`
- Command : `/note`

## Fichiers clés

| Fichier | Rôle |
|---------|------|
| `cmd/context-vault/main.go` | Serveur HTTP, schema SQLite, RingDumper, tous les handlers |
| `cmd/context-vault/mcp.go` | Serveur MCP stdio (JSON-RPC 2.0), 10 outils vault CRUD |
| `scripts/run.sh` | Lanceur SessionStart — démarre le binaire si port libre |
| `skills/prends-note/SKILL.md` | Skill : LLM inscrit des entités en DB de sa propre initiative |
| `skills/hot-contexte/SKILL.md` | Skill : SELECT ciblé avant compaction |
| `skills/project-mgmt/SKILL.md` | Skill : todos structurés avec dépendances |
| `skills/session-code/SKILL.md` | Skill : orchestration session dev (pick→steps→code→test→done) |
| `.claude/commands/note.md` | Commande `/note` — inscription par l'utilisateur |

## Types clés

| Type | Rôle |
|------|------|
| `Server` | État HTTP, pool DB, ring. Créé via `NewServer`. |
| `RingDumper` | Buffer circulaire fixe (10 entrées), mesure le volume accumulé |

## Build / test / deploy

```bash
CGO_ENABLED=0 go build -o bin/context-vault-$(go env GOOS)-$(go env GOARCH) ./cmd/context-vault
```

Pas de tests actuellement. Port : 9742. DB : `$PROJECT_DIR/.claude/vault.db`.

### Activer le serveur MCP

Créer un `.mcp.json` à la racine du projet cible (pas du repo context-vault) :

```json
{
  "mcpServers": {
    "context-vault": {
      "type": "stdio",
      "command": "/chemin/vers/context-vault/bin/context-vault-linux-amd64",
      "args": ["mcp", "-project", "/chemin/vers/projet"],
      "env": {}
    }
  }
}
```

- `command` : chemin absolu vers le binaire compilé
- `-project` : racine du projet — c'est là que `.claude/vault.db` sera créé
- Ce fichier est **machine-specific** (chemins absolus) → gitignored, jamais commité

## Convention de tracking

**Pas de modification de fichier sans vault todo préalable.** Le vault est le système de tracking unique. Créer la todo (même rapide, même une ligne) AVANT d'éditer un fichier. Ça garantit la traçabilité entre sessions et empêche les modifications orphelines.

## Invariants et pièges connus

- **DSN pragmas** : WAL, FK, busy_timeout via `_pragma=` dans le DSN — pas via `db.Exec("PRAGMA")`
- **MaxOpenConns(1)** : obligatoire — les pragmas DSN s'appliquent per-connection
- **stdout = signal** : `handleSessionStart` et `handleUserPrompt` écrivent sur stdout. Ce texte est injecté dans le contexte Claude Code. Ne jamais y écrire du debug.
- **PreCompact stdout non injecté** : `hot-contexte` doit être invoqué manuellement avant `/compact`
- **vault.db chmod 600** : appliqué à la création. `.gitignore` mis à jour automatiquement.
- **Pas de tests** : dette technique. RingDumper, handlers, ensureGitignore à couvrir.
- **Map rings bornée** : `Server.rings` est nettoyée sur session-end (`evictRing`). Safety net : si la map dépasse `maxRings` (50), `ringFor` purge tout via `clear`. Les données ring sont éphémères — la purge ne cause qu'un reset du compteur de tokens estimés.
- **3 modes d'exécution** : `main()` dispatch selon `os.Args[1]` — "user-prompt"/"session-start" (proxy stdin→HTTP), "mcp" (stdio JSON-RPC), default (HTTP server). Un seul binaire, 3 rôles.
- **handleSetup bloque la DB** : VACUUM peut prendre du temps sur une DB volumineuse. Appelé rarement (hook Setup).
