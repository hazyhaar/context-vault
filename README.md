# context-vault

Persistent structured memory for Claude Code.

The LLM is an active participant in its own memory — not a passive logger.
A Go server receives all hook events, writes to SQLite JSONB, and signals
Claude via `UserPromptSubmit` stdout when accumulated context exceeds the threshold.

## Architecture

```
Plugin hooks (HTTP)           Manual hook (command)        MCP stdio server
    SessionStart                  UserPromptSubmit          vault_upsert_entity
    PostToolUse                       ↓                    vault_list_todos
    Stop                     command hook proxy             vault_search_entities
    PreCompact               reads stdin, POSTs             vault_get_context
    SessionEnd               prints additionalContext       vault_todo_transition
        ↓                            ↓                     vault_create_relation
    POST localhost:9742  ←───────────┘                     vault_delete_entity
        ↓                                                       ↓
    Go server → SQLite JSONB (vault.db)  ←──────────────────────┘
```

### Post-compact flow

```
/compact → PreCompact HTTP hook → logged in buffer + compact_log
         → compaction happens
         → next UserPromptSubmit command hook
         → proxy POSTs to /hook/user-prompt
         → server detects pre_compact in buffer (sliding window)
         → buildStartContext → stdout plain text
         → Claude sees restored context in system-reminder
```

## Known Claude Code bugs (as of v2.1.70)

| Bug | Impact | Workaround |
|-----|--------|------------|
| **#10225** — `UserPromptSubmit` in plugin.json never executes | Cannot distribute via plugin marketplace | Manual `settings.json` entry |
| **#10373** — `SessionStart` stdout ignored for new sessions | No context injection at session start | Post-compact injection only |
| **#17550** — `additionalContext` JSON causes hook error | HTTP hooks cannot inject context | Plain text on stdout via command hook |

## Installation

### 1. Install the plugin

The plugin registers all HTTP hooks automatically (SessionStart, PostToolUse, Stop, etc.).

### 2. Add UserPromptSubmit manually (bug #10225)

Add to your project's `.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {"type": "command", "command": "/path/to/scripts/run.sh"},
          {"type": "command", "command": "/path/to/bin/context-vault-linux-amd64 session-start", "timeout": 5}
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {"type": "command", "command": "/path/to/bin/context-vault-linux-amd64 user-prompt", "timeout": 10}
        ]
      }
    ]
  }
}
```

Replace `/path/to/` with the actual plugin installation path.

## Binary — three modes

The same Go binary runs as:

1. **HTTP server** (default): `context-vault -project /path/to/project` — hooks server on `:9742`
2. **Command hook proxy**: `context-vault user-prompt` or `context-vault session-start` — reads stdin, POSTs to the HTTP server, prints `additionalContext` to stdout
3. **MCP stdio server**: `context-vault mcp -project /path/to/project` — JSON-RPC 2.0 over stdin/stdout, exposes 7 vault tools

## MCP server setup

The MCP server gives Claude direct access to vault tools (create/search/update entities, manage todos, create relations).

Create a `.mcp.json` at your **project root** (not inside the context-vault repo):

```json
{
  "mcpServers": {
    "context-vault": {
      "type": "stdio",
      "command": "/path/to/context-vault/bin/context-vault-linux-amd64",
      "args": ["mcp", "-project", "/path/to/your/project"],
      "env": {}
    }
  }
}
```

- `command`: absolute path to the compiled binary
- `-project`: project root — `vault.db` lives at `<project>/.claude/vault.db`
- `.mcp.json` contains machine-specific absolute paths — **do not commit it**

### Available MCP tools

| Tool | Description |
|------|-------------|
| `vault_get_context` | Active todos, decisions and constraints (session start view) |
| `vault_search_entities` | Search by type, namespace, or label substring |
| `vault_upsert_entity` | Create or update an entity |
| `vault_list_todos` | Todos with status, priority, deadline and blockers |
| `vault_todo_transition` | Change todo status (open/in_progress/blocked/done) |
| `vault_create_relation` | Link entities (depends_on, blocks, subtask_of) |
| `vault_delete_entity` | Delete entity and cascade relations |

See [USAGE.md](USAGE.md) for detailed examples.

## Skills

| Skill | Description |
|-------|-------------|
| **prends-note** | Persists information that must survive compaction. Claude invokes on its own initiative. User can also invoke via `/note`. |
| **hot-contexte** | Reasons about current state, completes DB if needed, returns targeted SELECT (~300 tokens). |
| **project-mgmt** | Structured todos with dependencies, priorities, deadlines. |

## Schema

SQLite JSONB. Dynamic — types and attributes emerge from usage.

- **sessions** — session lifecycle (id, started_at, ended_at, model)
- **entities** — identity + `meta` JSONB (blob_plus, blob_minus, status, deadline, ...)
- **relations** — typed edges (depends_on, blocks, subtask_of)
- **buffer** — raw hook payloads as JSONB (RingDumper input)
- **compact_log** — compaction reasoning and queries

Requires SQLite >= 3.45.0 (JSONB — January 2024).

## Build

```bash
CGO_ENABLED=0 go build -o bin/context-vault-$(go env GOOS)-$(go env GOARCH) ./cmd/context-vault
```

## Security

- `vault.db` is `chmod 600` on creation
- `.claude/vault.db` is automatically gitignored
- `sensitivity=2` entities are excluded from all context injection
- `type=credential` never stores values — only existence and location

## License

MIT
