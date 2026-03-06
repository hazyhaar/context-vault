# context-vault

Persistent structured memory for Claude Code.

The LLM is an active participant in its own memory — not a passive logger.
Skills give Claude the intention and tools to read and write to a local
SQLite database, in collaboration with the user.

## How it works

```
user + claude  →  /note or Claude initiative  →  prends-note  →  SQLite
                                                                     ↓
compaction imminent  →  hot-contexte  →  reasoning  →  targeted SELECT
                                                              ↓
                                             context for the next Claude
```

## Skills

| Skill | Description |
|-------|-------------|
| **prends-note** | Persists information that must survive compaction. Claude invokes this on its own initiative when it identifies something non-trivial. User can also invoke via `/note`. |
| **hot-contexte** | Invoked when compaction is imminent. Reasons about what the next Claude needs, fills gaps in DB, then builds a targeted SELECT query. |
| **project-mgmt** | Structured todos with dependencies, priorities, and deadlines. Persists across compactions and sessions. |

## Schema

EAV (Entity-Attribute-Value) model in SQLite. The ontology emerges from
usage — no fixed types or columns.

- **entities** — identity (namespace, type, label, sensitivity)
- **attributes** — dynamic key/value pairs per entity
- **relations** — typed edges between entities (depends_on, blocks, subtask_of)
- **sessions** — session lifecycle tracking
- **compact_log** — history of compaction reasoning and queries

## Installation

1. Clone or install the plugin in your Claude Code plugins directory
2. The `SessionStart` hook automatically:
   - Creates/migrates the SQLite DB at `.claude/vault.db`
   - Sets permissions to `600`
   - Adds `.claude/vault.db` to `.gitignore`

## Usage

### `/note` command

```
/note AFP meeting repoussé au 15
/note no CGO, modernc uniquement pour tout SQLite
/note todo : écrire les tests de SessionStart
```

Claude translates free text into structured INSERTs.

### Automatic note-taking

Claude invokes `prends-note` on its own when it identifies:
- A decision being made
- A constraint discovered
- A pattern rejected (blob_minus is as important as blob_plus)
- A file becoming central to the session
- A blocker emerging

### Project management

When multiple tasks emerge, Claude uses `project-mgmt` to create
structured todos with dependencies and priorities.

## Security

- `type=credential` never stores values — only existence and location
- `sensitivity=2` entities are excluded from all `hot-contexte` SELECTs
- DB file is `chmod 600` on creation
- `.claude/vault.db` is automatically gitignored

## License

MIT
