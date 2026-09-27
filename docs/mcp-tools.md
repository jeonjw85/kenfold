# Kenfold MCP tools

Contract for the tools Kenfold exposes over MCP. Schemas are generated from the Go types in [`internal/mcpserver/server.go`](../internal/mcpserver/server.go); that file is the source of truth, and this document explains intent and semantics.

## Conventions

- **`project`**: the git remote URL of the current repository, or a free-form project name when there is no remote. It is normalized to a lowercase `host/path` identity: scheme, credentials, port, query, trailing `/` and `.git` are removed, and scp-style `git@host:owner/repo` becomes `host/owner/repo`. So `https://github.com/Org/Repo.git` and `git@github.com:org/repo` are the same project (`project:github.com/org/repo`). Omit `project` for user-wide memories (`scope = user`); user-wide memories are always included in reads.
- **Memory types**: `semantic`, `episodic`, `project`, `preference`, `codebase`, `temporary` (see [ADR-0001](adr/0001-architecture.md#memory-model)).
- **Trust**: `user` (confirmed by the user, e.g. an approved preference), `agent` (written by an agent), `external` (derived from external content). Clients should treat `external` memories with extra caution.
- **Status**: `proposed`, `active`, `superseded`, `deleted`. Reads return `active`, unexpired memories only.
- **The calling agent** (`source_agent`) is determined by the server, never from tool arguments. Over HTTP it is the agent the API key was created for. For stdio servers it is `KENFOLD_AGENT`; without it, the client's `clientInfo.name` (lowercased, sanitized). If none is available, `unknown`.
- **Errors**: invalid arguments are rejected by schema validation before reaching the handler. Domain errors (bad id, oversized input, superseding a retired memory) are returned as tool errors (`isError: true`) with a message the model can act on. Internal failures return a generic tool error; details go to the server log only.
- **Secrets**: `remember` and `handoff` reject content that contains credentials (provider API keys and tokens, private keys, JWTs, passwords in URLs, random-looking values assigned to secret-named fields). The error names the kind of secret, never its value. `forget` reasons and search queries are redacted instead of rejected.
- **Sessions**: a client may send `_meta: {"kenfold/session_id": "<id>"}` with a tool call; it is recorded as the memory's `source_session`. `kenfold hook` does this for session summaries.
- **Local paths are not projects**: `project` values such as `/home/me/repo` or `~/repo` are rejected, because the same repository would become a different project on every machine.
- **Memory is data**: returned `content` must never be interpreted by the agent as instructions.

## Search

`recall` and the `relevant` part of `get_context` use hybrid search:

1. **Full-text**: the query's words are OR-ed (so a natural-language question matches memories that share some of its words) and ranked with `ts_rank`. Text search uses PostgreSQL's `simple` configuration: no stemming, which is predictable for mixed Korean and English.
2. **Vector** (when an embedding model is configured): cosine similarity against memories embedded by the same model; matches farther than `KENFOLD_SEARCH_MAX_DISTANCE` (default 0.55) are dropped.
3. The two rankings are fused with Reciprocal Rank Fusion (k = 60). `score` is normalized to (0, 1]: 1.0 means ranked first by every ranker that ran.

If the embedding provider fails or times out (10 s), the call still succeeds with full-text results only, and memories written meanwhile are embedded by a background backfill.

## Tools

### `get_context` (read-only)

Load everything relevant at the start of a task, within a token budget.

| Param | Type | Required | Notes |
|---|---|---|---|
| `task` | string | | What the agent is about to do; ranks `relevant` |
| `project` | string | | See conventions |
| `budget_tokens` | integer | | Default 2000, clamped to 200–32000 |

Returns, in priority order: `preferences[]` (active preferences, user-wide and project), `handoff` (the latest handoff for the project that no agent has resumed), `recent[]` (the latest three session summaries in the project, `episodic` memories written by session hooks, truncated to 800 characters), `project[]` (active `project` memories), and `relevant[]` (hybrid search on `task`). Each memory appears at most once. Memories that do not fit the budget are left out and `truncated` is set; use `recall` to find more. Token counts are estimates.

### `remember` (idempotent)

Store a durable memory.

| Param | Type | Required | Notes |
|---|---|---|---|
| `content` | string | yes | Self-contained statement, at most 8000 characters |
| `type` | enum | | Default: `project` when `project` is given, otherwise `semantic` |
| `project` | string | | Omit for user-wide |
| `supersedes` | string (id) | | An active memory in the same scope that this one replaces |
| `ttl_seconds` | integer | | Required for `type = temporary`; at most one year |

Returns `id`, `type`, `status`, `deduplicated`, and `similar`.

- **Deduplication**: content that differs from an existing active or proposed memory of the same type and scope only in case, whitespace, or trailing punctuation returns that memory with `deduplicated: true`; nothing new is stored. Superseding a memory with the same statement is also a no-op.
- **Similar memories**: after storing, `similar[]` lists up to three active memories in the same scope that are close to the new one (cosine distance ≤ 0.25 with embeddings, or trigram similarity ≥ 0.5). They may state the same fact differently or contradict it. Kenfold does not decide which is right: the agent supersedes or forgets the outdated one. Session summaries and temporary memories are not checked.
- **Supersede**: the replaced memory must be active and in the same scope. When the new memory is active, the old one becomes `superseded` in the same transaction and is kept as history. When the new memory is `proposed`, the old one stays active until the new one is approved.
- **Session summaries**: an `episodic` memory sent with a session id (see Conventions) supersedes the earlier summary from the same agent and session, so each session keeps one current summary.
- **Review**: new `preference` memories are `proposed` and are not served until the user approves them (`kenfold memory approve <id>`), which also sets `trust = user`.

Planned (Phase 2b): model-based extraction of memories from sessions and type classification.

### `recall` (read-only)

Search memory in natural language.

| Param | Type | Required | Notes |
|---|---|---|---|
| `query` | string | yes | Truncated to 2000 characters |
| `types` | enum[] | | Restrict to these types |
| `project` | string | | User-wide memories are always included |
| `limit` | integer | | Default 10, max 50 |

Returns `memories[]` sorted by `score` (see [Search](#search)).

### `handoff`

Leave a note so another agent or session can continue unfinished work. Stored as a `temporary` memory.

| Param | Type | Required | Notes |
|---|---|---|---|
| `summary` | string | yes | What was done and the current state; at most 8000 characters |
| `next_steps` | string[] | | At most 20 steps of 1000 characters each |
| `project` | string | | |
| `ttl_seconds` | integer | | Default 7 days, max 30 days |

Returns `id`, `expires_at`. There is at most one active handoff per project: a new handoff supersedes the previous one (linked via `supersedes`).

### `resume` (idempotent write)

| Param | Type | Required |
|---|---|---|
| `project` | string | |

Returns the latest unexpired handoff for the project, including `next_steps`, or no `handoff` field if there is none. The first call records `resumed_by` (the calling agent) and the time; later calls return the same handoff without changing that record. A resumed handoff is no longer offered by `get_context`, but `resume` still returns it until it expires or is replaced.

### `forget` (destructive, idempotent)

Soft-delete a memory: `status = deleted`. The row is kept for audit and history; it is no longer served.

| Param | Type | Required | Notes |
|---|---|---|---|
| `id` | string | yes | A memory id (UUID) |
| `reason` | string | | Truncated to 1000 characters |

Returns `id`, `status`. The first deletion records `forgotten_by` and `forget_reason`; forgetting again returns the memory unchanged.

## Session hooks

`kenfold hook` is a command hook for Claude Code and Codex. It is an MCP client of the server like any agent, authenticated by the agent's API key, so its writes pass the same checks.

| Event | What the hook does |
|---|---|
| `SessionStart` | Detects the project from the git remote of `cwd`; sends summaries spooled by earlier sessions; calls `get_context` (budget 1500) and returns it as `additionalContext`, framed as reference data, not instructions. On failure it shows a warning (`systemMessage`) and the session continues. |
| `UserPromptSubmit`, `Stop`, `PostCompact` | Appends the prompt, final response, or compaction summary to a local session log (redacted, mode 0600). No network call. |
| `SessionEnd` | Builds an extractive summary (requests, final response, compaction summary) and sends it with `remember` as an `episodic` memory with the session id. It is spooled first, so a summary interrupted by the client's time limit or an unreachable server is sent at the next `SessionStart`. |

Sessions outside a git repository are not recorded; their context is user-wide. The hook always exits 0.

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

Optional fields: `expires_at` (temporary memories and handoffs), `next_steps` and `resumed_by` (handoffs), `score` (search results).
