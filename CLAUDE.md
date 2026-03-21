> **Protocole** — Avant toute tâche, lire [`../CLAUDE.md`](../CLAUDE.md) §Protocole de recherche.
> Commandes obligatoires : `Read <dossier>/CLAUDE.md` → `Grep "CLAUDE:SUMMARY"` → `Grep "CLAUDE:WARN" <fichier>`.
> **Interdit** : Bash(grep/cat/find) au lieu de Grep/Read. Ne jamais lire un fichier entier en première intention.
> **context-vault MCP (beta)** — `vault_get_context({})` début de session · `vault_list_todos({})` backlog · `vault_upsert_entity` decisions/pièges · `vault_todo_transition` fin de tâche. Référence : [`USAGE.md`](USAGE.md)

# CLAUDE.md — context-vault

> **Outils MCP** : voir [`USAGE.md`](USAGE.md) pour la référence complète des 10 outils vault avec exemples.

## Responsabilité

Mémoire persistante inter-sessions pour Claude Code. Architecture daemon TCP (sole writer) + thin client MCP stdio (proxy).
Le daemon gère les connexions multi-terminaux, route les checkpoints entre workers et superviseur, persiste les agents.

## Architecture (mars 2026)

```
Claude Code ←stdio→ [thin client MCP] ←TCP:9743→ [daemon] ←SQL→ vault.db
                          │                          │
                     fallback SQLite          watchCheckpoints()
                     (si daemon down)         pushToRole/pushToSession
```

- **Daemon** (`cmd/context-vault-daemon/`) : TCP JSON-RPC localhost:9743, sole writer vault.db, checkpoint watcher 3s, table agents persistante
- **Thin client** (`cmd/context-vault-mcp/`) : MCP stdio, proxy vers daemon, fallback SQLite direct, channel forwarding
- **Legacy** (`cmd/context-vault/`) : ancien HTTP hooks server + MCP monolithique (deprecated, conservé pour compatibilité)
- **Vault core** (`internal/vault/`) : business logic MCP-indépendante, schema, migrations, types

## Dépendances

- `modernc.org/sqlite` — SQLite sans CGO

## Dépendants

- Claude Code (via `.mcp.json` — thin client MCP stdio)
- Skill `supervisor` (assume_role, channel push checkpoints)
- Skill `session-code` (vault todos, steps, checkpoints)

## Fichiers clés

| Fichier | Rôle |
|---------|------|
| `cmd/context-vault-daemon/main.go` | Daemon TCP, client tracking, checkpoint watcher, agents table, push notifications |
| `cmd/context-vault-mcp/main.go` | Thin client MCP stdio, TCP proxy, fallback SQLite, channel forwarding |
| `internal/vault/vault.go` | Business logic : CRUD entités, todos, steps, transitions, relations, checkpoints |
| `internal/vault/schema.go` | DDL vault.db + migrations (agents table, etc.) |
| `internal/vault/types.go` | Types retour MCP-indépendants (Result, Content) |
| `cmd/context-vault/main.go` | Legacy HTTP hooks server (deprecated) |
| `cmd/context-vault/mcp.go` | Legacy MCP monolithique (deprecated) |

## Types clés

| Type | Package | Rôle |
|------|---------|------|
| `Vault` | `internal/vault` | Business logic, tous les outils vault, sessionFn injectable |
| `daemon` | `cmd/context-vault-daemon` | État TCP server, clients map, mu sync |
| `clientInfo` | `cmd/context-vault-daemon` | Conn + encoder + sessionID + role par client |
| `daemonConn` | `cmd/context-vault-mcp` | Connexion TCP muxée (respCh + closed channel) |

## Build / test / deploy

```bash
# Daemon
CGO_ENABLED=0 go build -o bin/context-vault-daemon-linux-amd64 ./cmd/context-vault-daemon/

# Thin client MCP
CGO_ENABLED=0 go build -o bin/context-vault-mcp-linux-amd64 ./cmd/context-vault-mcp/

# Tests
go test -race -count=1 ./...

# Lancer le daemon
PROJECT_DIR=/devhoros nohup ./bin/context-vault-daemon-linux-amd64 -db /devhoros/.claude/vault.db &
```

DB : `$PROJECT_DIR/.claude/vault.db`. Daemon port : 9743.

### Configuration MCP (`.mcp.json`)

```json
{
  "mcpServers": {
    "context-vault": {
      "command": "/devhoros/context-vault/bin/context-vault-mcp-linux-amd64",
      "args": ["-role", "worker", "-channel"],
      "env": { "PROJECT_DIR": "/devhoros" }
    }
  }
}
```

- `-role worker|supervisor` : rôle initial du client
- `-channel` : active le forwarding des notifications daemon → MCP channel events
- `PROJECT_DIR` : racine projet (fallback SQLite si daemon down)

### Lancement Claude Code avec channels

```bash
claude --dangerously-skip-permissions --dangerously-load-development-channels server:context-vault
```

## Invariants et pièges connus

- **Daemon = sole writer** : toutes les écritures vault.db passent par le daemon. Le thin client est un proxy. Fallback SQLite = mode dégradé sans push.
- **Supervisor singleton** : un seul superviseur connecté à la fois (enforced via agents table). `vault_assume_role` rejette si un autre superviseur est actif.
- **Session restore** : `handleRegister` restaure le rôle depuis la table agents au reconnect. Le rôle demandé peut différer du rôle effectif (notify/role_restored).
- **watchCheckpoints goroutine** : poll vault.db toutes les 3s, maps notifiedCreated/notifiedAnswered croissent sans éviction. Watermarks basées sur timestamps unix.
- **MaxOpenConns(4)** dans le daemon (multi-reader). Le thin client fallback utilise les mêmes pragmas DSN.
- **Startup reset** : `UPDATE agents SET connected=0` au démarrage daemon — aucune connexion TCP n'existe encore.
- **callDaemon ID=1** : toutes les requêtes utilisent `id:1` — pas de pipelining concurrent.
- **DSN pragmas** : WAL, FK, busy_timeout, synchronous(NORMAL), _txlock=immediate via DSN — jamais `db.Exec("PRAGMA")`.

## NE PAS

- Écrire dans vault.db en contournant le daemon (sauf fallback quand daemon down)
- Lancer plusieurs instances du daemon (singleton, port 9743)
- Utiliser le legacy `cmd/context-vault` pour de nouveaux développements
- Oublier `-channel` dans la config MCP si les push notifications sont nécessaires
