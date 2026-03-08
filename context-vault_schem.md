# context-vault — Technical Schema

**HTTP hooks server — persists Claude Code session events to SQLite JSONB.**

Module: `github.com/hazyhaar/context-vault`
Go: 1.21 | Deps: modernc.org/sqlite | CGO_ENABLED=0
Binary: `cmd/context-vault` — listens on localhost:9742

## Arborescence

```
context-vault/
├── cmd/context-vault/
│   └── main.go              Server, RingDumper, handlers, schema
├── scripts/
│   └── run.sh               SessionStart launcher (health check + bg start)
├── skills/
│   ├── prends-note/SKILL.md  LLM-initiated entity persistence
│   ├── hot-contexte/SKILL.md Targeted SELECT before compaction
│   └── project-mgmt/SKILL.md Structured todos with dependencies
└── .claude/commands/
    └── note.md               /note user command
```

## Architecture

```
╔══════════════════════════════════════════════════════════╗
║                    Claude Code                           ║
║  hooks: SessionStart, UserPromptSubmit, PostToolUse,     ║
║         PostToolUseFailure, Stop, PreCompact,            ║
║         SessionEnd, Setup                                ║
╚═══════════════════════╤══════════════════════════════════╝
                        │ HTTP POST localhost:9742
                        ▼
╔══════════════════════════════════════════════════════════╗
║                      Server                              ║
║                                                          ║
║  db         *sql.DB     (MaxOpenConns=1)                 ║
║  ring       *RingDumper (circular buffer, 10 entries)    ║
║  sessionID  string      (guarded by mu)                  ║
╠══════════════════════════════════════════════════════════╣
║  /hook/session-start  → INSERT sessions + body: context  ║
║  /hook/user-prompt    → INSERT buffer + body: signal     ║
║  /hook/post-tool      → INSERT buffer                    ║
║  /hook/post-tool-fail → INSERT buffer                    ║
║  /hook/stop           → INSERT buffer                    ║
║  /hook/pre-compact    → INSERT buffer + compact_log      ║
║  /hook/session-end    → UPDATE sessions.ended_at         ║
║  /hook/setup          → PRAGMA optimize + VACUUM         ║
║  /health              → 200 OK                           ║
╚═══════════════╤════════════════════╤═════════════════════╝
                │                    │
                ▼                    ▼
╔═══════════════════════╗  ╔════════════════════════════╗
║     RingDumper        ║  ║  HTTP response body        ║
║                       ║  ║  (injected by Claude Code) ║
║  buf [10]entry        ║  ║                            ║
║  head, total int      ║  ║  SessionStart:             ║
║  mu sync.Mutex        ║  ║    types summary, todos,   ║
║                       ║  ║    constraints              ║
║  Push() → evict+add   ║  ║                            ║
║  ShouldSignal()       ║  ║  UserPromptSubmit:         ║
║    total/4 > 2000     ║  ║    "[vault] ~N tokens"     ║
╚═══════════════════════╝  ╚════════════════════════════╝
```

## Schéma de données

```
╔══════════════════════════════════════════════════════════╗
║  TABLE: sessions                                         ║
╠═══════════════╤═══════════╤══════════════════════════════╣
║  id           │ TEXT      │ PK                           ║
║  started_at   │ INTEGER   │ NOT NULL, unix epoch         ║
║  ended_at     │ INTEGER   │ set on SessionEnd            ║
║  project      │ TEXT      │ project directory            ║
║  model        │ TEXT      │ Claude model used            ║
╚═══════════════╧═══════════╧══════════════════════════════╝

╔══════════════════════════════════════════════════════════╗
║  TABLE: entities                                         ║
╠═══════════════╤═══════════╤══════════════════════════════╣
║  id           │ INTEGER   │ PK                           ║
║  namespace    │ TEXT      │ NOT NULL                     ║
║  type         │ TEXT      │ NOT NULL (function, todo...) ║
║  label        │ TEXT      │ NOT NULL                     ║
║  sensitivity  │ INTEGER   │ DEFAULT 0 (2=excluded)       ║
║  ts_created   │ INTEGER   │ NOT NULL                     ║
║  ts_updated   │ INTEGER   │ NOT NULL                     ║
║  session_origin│ TEXT     │ session that created it      ║
║  meta         │ BLOB      │ JSONB (blob_plus, status...) ║
╚═══════════════╧═══════════╧══════════════════════════════╝
  IDX: (namespace, type), (ts_updated DESC)

╔══════════════════════════════════════════════════════════╗
║  TABLE: relations                                        ║
╠═══════════════╤═══════════╤══════════════════════════════╣
║  id           │ INTEGER   │ PK                           ║
║  from_id      │ INTEGER   │ FK → entities ON DELETE CASC ║
║  to_id        │ INTEGER   │ FK → entities ON DELETE CASC ║
║  type         │ TEXT      │ NOT NULL (depends_on, etc.)  ║
║  ts_created   │ INTEGER   │ NOT NULL                     ║
╚═══════════════╧═══════════╧══════════════════════════════╝
  IDX: (from_id), (to_id)

╔══════════════════════════════════════════════════════════╗
║  TABLE: buffer                                           ║
╠═══════════════╤═══════════╤══════════════════════════════╣
║  id           │ INTEGER   │ PK                           ║
║  session_id   │ TEXT      │ NOT NULL                     ║
║  ts           │ INTEGER   │ NOT NULL                     ║
║  hook         │ TEXT      │ NOT NULL (event type)        ║
║  payload      │ BLOB      │ NOT NULL, raw JSONB          ║
║  size_est     │ INTEGER   │ NOT NULL, byte estimate      ║
║  processed    │ INTEGER   │ DEFAULT 0                    ║
╚═══════════════╧═══════════╧══════════════════════════════╝
  IDX: (session_id, processed, ts)

╔══════════════════════════════════════════════════════════╗
║  TABLE: compact_log                                      ║
╠═══════════════╤═══════════╤══════════════════════════════╣
║  id           │ INTEGER   │ PK                           ║
║  ts           │ INTEGER   │ NOT NULL                     ║
║  session_id   │ TEXT      │ NOT NULL                     ║
║  trigger      │ TEXT      │ auto / manual                ║
║  reasoning    │ TEXT      │ hot-contexte step 1 output   ║
║  query_used   │ TEXT      │ SELECT executed              ║
║  result_text  │ TEXT      │ extracted context             ║
╚═══════════════╧═══════════╧══════════════════════════════╝
```

## Flux de données

### Hook ingestion (every event)

```
Claude Code hook event
  │
  ▼
┌──────────────┐                    ┌──────────────┐
│ readBody()   │ ──────────────────▶│ jsonPayload() │
│ io.LimitRead │                    │ validate JSON │
└──────────────┘                    └──────┬───────┘
                                          │
                                          ▼
                                   ┌──────────────┐
                                   │ INSERT buffer │
                                   │ as JSONB      │
                                   └──────┬───────┘
                                          │
                                          ▼
                                   ┌──────────────┐
                                   │ ring.Push()  │
                                   │ update total │
                                   └──────────────┘
```

### Compaction signal (UserPromptSubmit only)

```
ring.ShouldSignal()
  │ total/4 > 2000?
  │
  ├── no  → (silent)
  │
  └── yes → HTTP response body "[vault] ~N tokens"
              │
              ▼
         Claude Code injects response into context
         Claude sees signal, invokes prends-note
```

### Session start (context injection)

```
SessionStart hook
  │
  ▼
┌────────────────────┐
│ INSERT sessions    │
│ ensureGitignore()  │
└────────┬───────────┘
         │
         ▼
┌────────────────────┐
│ buildStartContext()│
│ 3x QueryContext    │
│ types, todos,      │
│ constraints        │
└────────┬───────────┘
         │
         ▼
    HTTP response body → injected by Claude Code
```

## Types publics

```
╔═════════════════════════════════════════════════════╗
║  type Server struct {                               ║
║      db         *sql.DB      // MaxOpenConns=1      ║
║      ring       *RingDumper  // set once             ║
║      projectDir string                              ║
║      sessionID  string       // guarded by mu        ║
║      mu         sync.RWMutex                        ║
║  }                                                  ║
╠═════════════════════════════════════════════════════╣
║  type RingDumper struct {                           ║
║      mu    sync.Mutex                               ║
║      buf   [10]entry   // fixed ring                ║
║      head  int         // next write position        ║
║      total int         // sum of all entry sizes     ║
║  }                                                  ║
╚═════════════════════════════════════════════════════╝
```
