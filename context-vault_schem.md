# context-vault — Technical Schema

**Daemon TCP mémoire persistante inter-sessions pour Claude Code.**

Module: `github.com/hazyhaar/context-vault`
Go: 1.25 | Deps: modernc.org/sqlite | CGO_ENABLED=0
Binaires: `cmd/context-vault-daemon` (TCP:9743), `cmd/context-vault-mcp` (MCP stdio)

## Arborescence

```
context-vault/
├── cmd/
│   ├── context-vault-daemon/
│   │   └── main.go             Daemon TCP, client tracking, checkpoint watcher
│   ├── context-vault-mcp/
│   │   └── main.go             Thin client MCP stdio, TCP proxy, fallback SQLite
│   └── context-vault/
│       ├── main.go             Legacy HTTP hooks server (deprecated)
│       └── mcp.go              Legacy MCP monolithique (deprecated)
├── internal/vault/
│   ├── vault.go                Business logic — 16 opérations vault
│   ├── schema.go               DDL vault.db + migrations
│   └── types.go                Result, Content — types retour MCP-indépendants
├── skills/
│   ├── session-code/SKILL.md   Orchestration dev (pick→steps→code→test→done)
│   └── supervisor/SKILL.md     Supervision multi-terminaux
└── scripts/
    └── run.sh                  SessionStart launcher (legacy)
```

## Architecture

```
┌─────────────────┐   ┌─────────────────┐   ┌─────────────────┐
│  Claude Code    │   │  Claude Code    │   │  Claude Code    │
│  (supervisor)   │   │  (worker 1)     │   │  (worker 2)     │
└────────┬────────┘   └────────┬────────┘   └────────┬────────┘
         │ stdio               │ stdio               │ stdio
┌────────▼────────┐   ┌────────▼────────┐   ┌────────▼────────┐
│  thin client    │   │  thin client    │   │  thin client    │
│  -role supvsr   │   │  -role worker   │   │  -role worker   │
│  -channel       │   │  -channel       │   │  -channel       │
└────────┬────────┘   └────────┬────────┘   └────────┬────────┘
         │ TCP:9743            │ TCP:9743            │ TCP:9743
         └────────────┬────────┴────────────┬────────┘
                      │                     │
              ╔═══════▼═════════════════════▼═══════╗
              ║          daemon                     ║
              ║                                     ║
              ║  mu sync.RWMutex                    ║
              ║  clients map[net.Conn]*clientInfo    ║
              ║  v *vault.Vault                     ║
              ║                                     ║
              ║  ┌──────────────────────────────┐   ║
              ║  │ watchCheckpoints (goroutine)  │   ║
              ║  │ poll 3s → pushToRole/Session  │   ║
              ║  └──────────────────────────────┘   ║
              ║                                     ║
              ║  ┌──────────────────────────────┐   ║
              ║  │ WAL checkpoint (goroutine)    │   ║
              ║  │ PASSIVE every 2min            │   ║
              ║  └──────────────────────────────┘   ║
              ╚═══════════════╤═════════════════════╝
                              │ SQL
                      ╔═══════▼═══════╗
                      ║   vault.db    ║
                      ║   SQLite WAL  ║
                      ╚═══════════════╝
```

## Thin client — TCP mux

```
stdin (Claude Code)                     TCP conn (daemon)
      │                                       │
      │  JSON-RPC request                     │
      ├──────────────────────────────────────▶│
      │                                       │
      │                          ┌────────────┤
      │                          │ startMux   │
      │                          │ goroutine  │
      │                          └────┬───────┘
      │                               │
      │  ◀── response (has id) ───────┤ respCh
      │  ◀── notification (no id) ────┤ onNotif → MCP channel event
      │                               │
      ▼                               ▼
stdout (MCP response / channel)   dc.closed on disconnect
```

Fallback : si daemon injoignable → `openFallbackVault()` → SQLite direct (sans push).

## Schéma de données (vault.db)

```
╔══════════════════════════════════════════════════════════╗
║  TABLE: entities                                        ║
╠════════════════╤════════════╤════════════════════════════╣
║  id            │ INTEGER    │ PRIMARY KEY                ║
║  namespace     │ TEXT       │ NOT NULL                   ║
║  type          │ TEXT       │ NOT NULL                   ║
║  label         │ TEXT       │ NOT NULL                   ║
║  sensitivity   │ INTEGER    │ DEFAULT 0 (0/1/2)         ║
║  ts_created    │ INTEGER    │ NOT NULL (unix)            ║
║  ts_updated    │ INTEGER    │ NOT NULL (unix)            ║
║  session_origin│ TEXT       │ session qui a créé         ║
║  meta          │ BLOB       │ JSONB (status, priority…) ║
╚════════════════╧════════════╧════════════════════════════╝

╔══════════════════════════════════════════════════════════╗
║  TABLE: relations                                       ║
╠════════════════╤════════════╤════════════════════════════╣
║  id            │ INTEGER    │ PRIMARY KEY                ║
║  from_id       │ INTEGER    │ FK entities ON DELETE CASC ║
║  to_id         │ INTEGER    │ FK entities ON DELETE CASC ║
║  type          │ TEXT       │ depends_on/blocks/subtask  ║
║  ts_created    │ INTEGER    │ NOT NULL                   ║
╚════════════════╧════════════╧════════════════════════════╝

╔══════════════════════════════════════════════════════════╗
║  TABLE: todo_steps                                      ║
╠════════════════╤════════════╤════════════════════════════╣
║  todo_id       │ INTEGER    │ FK entities ON DELETE CASC ║
║  step          │ TEXT       │ NOT NULL                   ║
║  step_key      │ TEXT       │ NOT NULL (dedup key)       ║
║  required      │ INTEGER    │ DEFAULT 1                  ║
║  done          │ INTEGER    │ DEFAULT 0                  ║
║  done_at       │ INTEGER    │                            ║
║  PRIMARY KEY (todo_id, step_key)                        ║
╚════════════════╧════════════╧════════════════════════════╝

╔══════════════════════════════════════════════════════════╗
║  TABLE: agents                                          ║
╠════════════════╤════════════╤════════════════════════════╣
║  session_id    │ TEXT       │ PRIMARY KEY                ║
║  role          │ TEXT       │ DEFAULT 'worker'           ║
║  connected     │ INTEGER    │ 0/1                        ║
║  registered_at │ INTEGER    │ NOT NULL (unix)            ║
║  last_seen_at  │ INTEGER    │ NOT NULL (unix)            ║
╚════════════════╧════════════╧════════════════════════════╝

╔══════════════════════════════════════════════════════════╗
║  TABLE: buffer  (legacy — HTTP hooks)                   ║
╠════════════════╤════════════╤════════════════════════════╣
║  id            │ INTEGER    │ PRIMARY KEY                ║
║  session_id    │ TEXT       │ NOT NULL                   ║
║  ts            │ INTEGER    │ NOT NULL                   ║
║  hook          │ TEXT       │ NOT NULL                   ║
║  payload       │ BLOB       │ NOT NULL (JSONB)           ║
║  size_est      │ INTEGER    │ NOT NULL                   ║
║  processed     │ INTEGER    │ DEFAULT 0                  ║
╚════════════════╧════════════╧════════════════════════════╝

╔══════════════════════════════════════════════════════════╗
║  TABLE: sessions  (legacy — HTTP hooks)                 ║
╠════════════════╤════════════╤════════════════════════════╣
║  id            │ TEXT       │ PRIMARY KEY                ║
║  started_at    │ INTEGER    │ NOT NULL                   ║
║  ended_at      │ INTEGER    │                            ║
║  project       │ TEXT       │                            ║
║  model         │ TEXT       │                            ║
╚════════════════╧════════════╧════════════════════════════╝
```

## Flux — checkpoint routing

```
Worker                         Daemon                      Supervisor
  │                              │                              │
  │ vault_upsert_entity          │                              │
  │ (type: checkpoint)           │                              │
  ├─────────────────────────────▶│                              │
  │                              │ INSERT entities              │
  │                              │                              │
  │                              │ watchCheckpoints (3s poll)   │
  │                              │ PollNewCheckpoints           │
  │                              │                              │
  │                              │ pushToRole("supervisor")     │
  │                              ├─────────────────────────────▶│
  │                              │  notify/checkpoint_created   │
  │                              │                              │
  │                              │                              │ vault_upsert_entity
  │                              │                              │ (answer in meta)
  │                              │◀─────────────────────────────┤
  │                              │ UPDATE entities              │
  │                              │                              │
  │                              │ PollAnsweredCheckpoints      │
  │  notify/checkpoint_answered  │                              │
  │◀─────────────────────────────┤ pushToSession(origin)        │
  │                              │                              │
```

## Flux — role management

```
Client connect
  │
  ▼
register(session_id, role)
  │
  ├── session_id exists in agents?
  │     yes → restore role from DB, push notify/role_restored
  │     no  → INSERT agents
  │
  ▼
assume_role(role, session_id?)
  │
  ├── role = supervisor?
  │     yes → check singleton (agents WHERE role=supervisor AND connected=1)
  │           already active → reject (-32603)
  │           none → UPDATE agents, grant role
  │
  ├── downgrade from supervisor?
  │     yes → post-action: checkNoSupervisor()
  │           if count(supervisor, connected=1) = 0 → broadcast notify/no_supervisor
  │
  ▼
disconnect (removeClient)
  │
  ├── UPDATE agents SET connected=0
  └── was supervisor? → checkNoSupervisor()
```

## Types publics (internal/vault)

```
╔═══════════════════════════════════════════════╗
║  type Vault struct {                          ║
║      DB        *sql.DB                        ║
║      SessionFn func() string                  ║
║  }                                            ║
╠═══════════════════════════════════════════════╣
║  type Result struct {                         ║
║      Content []Content                        ║
║      IsError bool                             ║
║  }                                            ║
╠═══════════════════════════════════════════════╣
║  type Content struct {                        ║
║      Type string  // "text"                   ║
║      Text string                              ║
║  }                                            ║
╚═══════════════════════════════════════════════╝

16 opérations : GetContext, SearchEntities, UpsertEntity,
TodoTransition, ListTodos, CreateRelation, DeleteEntity,
AddSteps, StepDone, GetEntity, WithSession,
PollNewCheckpoints, PollAnsweredCheckpoints,
missionChildIDs (private), TextResult, ErrorResult
```
