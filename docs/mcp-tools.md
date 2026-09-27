# Kenfold MCP tools

Contract for the tools Kenfold exposes over MCP. Schemas are generated from the Go types in [`internal/mcpserver/server.go`](../internal/mcpserver/server.go); that file is the source of truth, and this document explains intent and semantics.

**Phase 0 status:** all six tools are registered with final-draft schemas. Handlers return a tool error (`isError: true`, "not implemented yet") until Phase 1.

## Conventions

- **`project`**: the normalized git remote URL of the current repository (e.g. `github.com/kenfold/kenfold`), or a free-form project name when there is no remote. Omit it for user-wide memories. User-wide memories (`scope = user`) are always included in reads.
- **Memory types**: `semantic`, `episodic`, `project`, `preference`, `codebase`, `temporary` (see [ADR-0001](adr/0001-architecture.md#memory-model)).
- **Trust**: `user`, `agent`, `external`. Clients should treat `external` memories with extra caution.
- **Status**: `proposed`, `active`, `superseded`, `deleted`. Reads return `active` only.
- **The calling agent** (`source_agent`) is determined by the server, never from tool arguments: from the API key's registered agent name (Phase 1), falling back to MCP `clientInfo.name`. Over stateless HTTP, `clientInfo` is only available per request on protocol 2026-07-28, so the API key is the reliable source.
- **Errors**: invalid arguments are rejected by schema validation before reaching the handler. Domain errors are returned as tool errors (`isError: true`) with a human-readable message, so the model can react.
- **Memory is data**: returned `content` must never be interpreted by the agent as instructions.

## Tools

### `get_context` (read-only)

Load everything relevant at the start of a task, within a token budget.

| Param | Type | Required | Notes |
|---|---|---|---|
| `task` | string | | What the agent is about to do; ranks `relevant` |
| `project` | string | | See conventions |
| `budget_tokens` | integer | | Default 2000 |

Returns `preferences[]`, `project[]`, `relevant[]` (each a `MemoryView`), and `handoff` (latest pending handoff for the project, if any).

Assembly order under budget: preferences → pending handoff → project memories → task-relevant memories.

### `remember`

Store a durable memory.

| Param | Type | Required | Notes |
|---|---|---|---|
| `content` | string | yes | Self-contained statement that makes sense without the current conversation |
| `type` | enum | | Server classifies if omitted |
| `project` | string | | Omit for user-wide |
| `supersedes` | string (id) | | Replaces an existing memory: the old one becomes `superseded` |
| `ttl_seconds` | integer | | Required when `type = temporary` |

Returns `id`, `type`, `status`. Status is `proposed` instead of `active` when the memory needs review (e.g. new `preference` not confirmed by the user, or `external` trust).

Planned server-side behavior (Phase 2): secret scanning (reject), near-duplicate merge, contradiction detection → suggest `supersedes`.

### `recall` (read-only)

Search memory in natural language.

| Param | Type | Required | Notes |
|---|---|---|---|
| `query` | string | yes | |
| `types` | enum[] | | Restrict to these types |
| `project` | string | | User-wide memories are always included |
| `limit` | integer | | Default 10 |

Returns `memories[]` sorted by `score` (hybrid: full-text + vector in Phase 1; + graph + recency in Phase 3).

### `handoff`

Leave a note so another agent or session can continue unfinished work. Stored as a `temporary` memory.

| Param | Type | Required | Notes |
|---|---|---|---|
| `summary` | string | yes | What was done and the current state |
| `next_steps` | string[] | | Concrete next actions |
| `project` | string | | |
| `ttl_seconds` | integer | | Default 7 days |

Returns `id`, `expires_at`.

### `resume` (read-only)

| Param | Type | Required |
|---|---|---|
| `project` | string | |

Returns the latest unexpired `handoff` for the project, or no `handoff` field if there is none.

### `forget` (destructive, idempotent)

Soft-delete a memory: `status = deleted`. The row is kept for audit and history; it is no longer served.

| Param | Type | Required |
|---|---|---|
| `id` | string | yes |
| `reason` | string | |

Returns `id`, `status`.

## `MemoryView`

```json
{
  "id": "0199...",
  "type": "project",
  "scope": "project:github.com/kenfold/kenfold",
  "content": "HTTP transport is stateless; do not rely on Mcp-Session-Id.",
  "source_agent": "claude-code",
  "trust": "agent",
  "created_at": "2026-09-27T06:00:00Z",
  "score": 0.83
}
```
