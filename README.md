# context-vault

Persistent structured memory for Claude Code.

The LLM is an active participant in its own memory — not a passive logger.
A daemon TCP server is the sole writer to vault.db. Thin MCP clients connect
via TCP and proxy tool calls. Multi-terminal: workers create checkpoints,
the supervisor answers them. Push notifications route events in real time.

## Architecture

```
Claude Code ←stdio→ [thin client MCP] ←TCP:9743→ [daemon] ←SQL→ vault.db
                     cmd/context-vault-mcp         cmd/context-vault-daemon
                          │                              │
                     fallback SQLite              watchCheckpoints (3s)
                     (if daemon down)             pushToRole / pushToSession
                          │                              │
                     -role worker|supervisor       table agents (persistent)
                     -channel (push forwarding)    supervisor singleton
```

### Multi-terminal supervision

```
Terminal 1 (supervisor)          Terminal 2 (worker)          Terminal 3 (worker)
vault_assume_role(supervisor) →  vault_assume_role(worker) →  vault_assume_role(worker)
        ↑                               │                            │
        │ push checkpoint_created       │ vault_upsert_entity        │
        │←──────────────────────────────┘ (type: checkpoint)         │
        │                                                            │
        │ push checkpoint_answered                                   │
        │────────────────────────────────────────────────────────────→│
```

## Installation

### 1. Build

```bash
CGO_ENABLED=0 go build -o bin/context-vault-daemon-linux-amd64 ./cmd/context-vault-daemon/
CGO_ENABLED=0 go build -o bin/context-vault-mcp-linux-amd64 ./cmd/context-vault-mcp/
```

### 2. Start the daemon

```bash
PROJECT_DIR=/path/to/project nohup ./bin/context-vault-daemon-linux-amd64 -db /path/to/project/.claude/vault.db &
```

### 3. Configure MCP

Create `.mcp.json` at your **project root**:

```json
{
  "mcpServers": {
    "context-vault": {
      "command": "/path/to/bin/context-vault-mcp-linux-amd64",
      "args": ["-role", "worker", "-channel"],
      "env": { "PROJECT_DIR": "/path/to/project" }
    }
  }
}
```

### 4. Launch Claude Code with channels

```bash
claude --dangerously-skip-permissions --dangerously-load-development-channels server:context-vault
```

## Available MCP tools

| Tool | Description |
|------|-------------|
| `vault_get_context` | Active todos, decisions, constraints, checkpoints |
| `vault_search_entities` | Search by type, namespace, or label substring |
| `vault_upsert_entity` | Create or update an entity |
| `vault_list_todos` | Todos with status, priority, blockers, steps |
| `vault_todo_transition` | Change todo status (open/in_progress/blocked/done) |
| `vault_create_relation` | Link entities (depends_on, blocks, subtask_of) |
| `vault_delete_entity` | Delete entity and cascade relations |
| `vault_add_steps` | Add steps to a todo (idempotent) |
| `vault_step_done` | Mark a step done (auto-transition if all required done) |
| `vault_get_entity` | Read full entity by ID (meta, relations, steps) |
| `vault_list_workers` | List connected clients (supervisor only) |
| `vault_assume_role` | Change session role (worker/supervisor singleton) |

See [USAGE.md](USAGE.md) for detailed examples.

## Entity types

`todo`, `mission`, `decision`, `constraint`, `checkpoint`, `pattern`, `function`, `type`, `package`, `file`, `api`, `dependency`, `credential`

## Schema

SQLite WAL. Core tables:

- **entities** — identity + `meta` JSONB (blob_plus, blob_minus, status, priority, ...)
- **relations** — typed edges (depends_on, blocks, subtask_of)
- **todo_steps** — ordered steps per todo with done flag
- **agents** — persistent client registry (session_id, role, connected, timestamps)
- **sessions** / **buffer** / **compact_log** — legacy hook tables (still used by cmd/context-vault)

## Security

- `vault.db` is `chmod 600` on creation
- `.claude/vault.db` is automatically gitignored
- `sensitivity=2` entities are excluded from context injection
- `type=credential` never stores values — only existence and location
- Supervisor singleton enforced via agents table

## License

MIT
