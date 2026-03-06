# context-vault

Persistent structured memory for Claude Code.

The LLM is an active participant in its own memory — not a passive logger.
An HTTP server receives all hook events, writes to SQLite JSONB, and signals
Claude via `UserPromptSubmit` stdout when accumulated context exceeds the threshold.

## Architecture

```
hooks  →  POST localhost:9742/hook  →  Go server  →  SQLite JSONB
                                             ↓
                                      UserPromptSubmit
                                      RingDumper measure
                                      stdout if threshold exceeded
                                             ↓
                                      Claude sees signal
                                      invokes prends-note or hot-contexte
```

## Hooks used

| Hook | Role |
|------|------|
| `SessionStart` | Create session, chmod 600, .gitignore, inject context summary |
| `UserPromptSubmit` | Log prompt, RingDumper push, signal if threshold exceeded |
| `PostToolUse` | Buffer full payload as JSONB |
| `PostToolUseFailure` | Buffer tool errors (fragility patterns) |
| `Stop` | Buffer last_assistant_message |
| `PreCompact` | Buffer trigger + custom_instructions (hot-contexte invoked manually) |
| `SessionEnd` | Close session timestamp |
| `Setup` | VACUUM + PRAGMA optimize |

## Skills

| Skill | Description |
|-------|-------------|
| **prends-note** | Persists information that must survive compaction. Claude invokes on its own initiative. User can also invoke via `/note`. |
| **hot-contexte** | Invoked when compaction is imminent. Reasons about what next Claude needs, builds targeted SELECT. |
| **project-mgmt** | Structured todos with dependencies, priorities, deadlines. Persists across compactions and sessions. |

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
# Install dependency (requires network)
go mod download

# Build for current platform
go build -o bin/context-vault-$(go env GOOS)-$(go env GOARCH) ./cmd/context-vault

# Cross-compile
GOOS=darwin  GOARCH=arm64  go build -o bin/context-vault-darwin-arm64  ./cmd/context-vault
GOOS=darwin  GOARCH=amd64  go build -o bin/context-vault-darwin-amd64  ./cmd/context-vault
GOOS=linux   GOARCH=amd64  go build -o bin/context-vault-linux-amd64   ./cmd/context-vault
GOOS=linux   GOARCH=arm64  go build -o bin/context-vault-linux-arm64   ./cmd/context-vault
```

## Installation

Configure `.claude/settings.json` in your project:

```json
{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "scripts/run.sh"}, {"type": "http", "url": "http://localhost:9742/hook/session-start", "timeout": 5}]}],
    "UserPromptSubmit": [{"hooks": [{"type": "http", "url": "http://localhost:9742/hook/user-prompt", "timeout": 5}]}],
    "PostToolUse": [{"hooks": [{"type": "http", "url": "http://localhost:9742/hook/post-tool", "timeout": 5}]}],
    "PostToolUseFailure": [{"hooks": [{"type": "http", "url": "http://localhost:9742/hook/post-tool-failure", "timeout": 5}]}],
    "Stop": [{"hooks": [{"type": "http", "url": "http://localhost:9742/hook/stop", "timeout": 5}]}],
    "PreCompact": [{"hooks": [{"type": "http", "url": "http://localhost:9742/hook/pre-compact", "timeout": 10}]}],
    "SessionEnd": [{"hooks": [{"type": "http", "url": "http://localhost:9742/hook/session-end", "timeout": 5}]}],
    "Setup": [{"hooks": [{"type": "http", "url": "http://localhost:9742/hook/setup", "timeout": 30}]}]
  }
}
```

## Security

- `vault.db` is `chmod 600` on creation
- `.claude/vault.db` is automatically gitignored
- `sensitivity=2` entities are excluded from all `hot-contexte` SELECTs
- `type=credential` never stores values — only existence and location

## Known unknowns (validate before v1)

1. **PreCompact stdout** — is it injected into the compacted context? Sources say no. If confirmed, hot-contexte must be invoked manually before `/compact`.
2. **HTTP hook timeout 5s** — sufficient for a SQLite INSERT under load?
3. **modernc SQLite version** — confirm `>= 3.45.0` with `go list -m modernc.org/sqlite`
4. **UserPromptSubmit stdout max length** — short signal `[vault] ~N tokens` has no risk.

## License

MIT
