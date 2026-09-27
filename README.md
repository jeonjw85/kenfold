# Kenfold

**One memory for all your AI agents.** Bring every agent into the fold.

Claude Code, Codex, OpenCode, ChatGPT, and local models each keep their own memory. Kenfold is a self-hosted memory server they all share over [MCP](https://modelcontextprotocol.io): a decision made in one tool is known in the next.

```
Claude Code ─┐
Codex ───────┤
OpenCode ────┼── Kenfold ── PostgreSQL + pgvector
ChatGPT ─────┤
Local LLM ───┘
```

What makes it different (see [ADR-0001](docs/adr/0001-architecture.md)):

- **Typed memory**: semantic, episodic, project, preference, codebase, temporary, each with its own lifecycle
- **Provenance**: every memory records which agent wrote it (from its API key, not from what the client claims), when, and how far to trust it
- **No silent overwrites**: a replacement supersedes the old memory, which is kept as history
- **Handoff** between agents: stop in Claude Code, resume in Codex
- **Poisoning-aware**: memory is served as data, never as instructions; preferences need your approval

> **Status: Phase 2.** Shared storage, hybrid search (full-text + vectors), API keys, setup for Claude Code, Codex, and OpenCode, a server-side **secret filter**, duplicate and contradiction hints, and **session hooks** that load memory at session start and record a summary at session end. Not yet: model-based extraction and classification of memories from conversations.

## Quickstart

Requirements: Docker with Compose. Go 1.27+ only for development.

```sh
make up                        # Postgres 18 + pgvector and Kenfold on http://127.0.0.1:7077
make key AGENT=claude-code     # prints an API key for Claude Code (shown once)
make key AGENT=codex           # one key per agent, so every memory is attributed correctly
```

`make up` gives full-text search. For hybrid search, which also finds memories phrased differently from the query, run a local embedding model instead:

```sh
make up-embed                  # adds Ollama with bge-m3 (first run downloads ~1.2 GB)
```

Memories written without embeddings are embedded automatically once a model is available.

## Connect your agents

Each agent uses its own key. The key decides the agent name recorded on every memory.

**Claude Code**

```sh
claude mcp add --scope user --transport http kenfold http://127.0.0.1:7077/mcp \
  --header "Authorization: Bearer kf_..."
```

**Codex** (`~/.codex/config.toml`); set `KENFOLD_CODEX_KEY` in the environment Codex starts from:

```toml
[mcp_servers.kenfold]
url = "http://127.0.0.1:7077/mcp"
bearer_token_env_var = "KENFOLD_CODEX_KEY"
```

For the desktop app or IDE extension, which may not see your shell environment, use `http_headers = { "Authorization" = "Bearer kf_..." }` instead.

**OpenCode** (`opencode.json` in a project, or `~/.config/opencode/opencode.json`):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "kenfold": {
      "type": "remote",
      "url": "http://127.0.0.1:7077/mcp",
      "oauth": false,
      "headers": { "Authorization": "Bearer {env:KENFOLD_OPENCODE_KEY}" }
    }
  }
}
```

**stdio** (the client starts Kenfold as a subprocess): build with `make build`, then run `bin/kenfold mcp` with `KENFOLD_AGENT` set to the agent's name. The stdio server connects to the database directly (`KENFOLD_DATABASE_URL`, default: the compose database), so it does not use API keys and the agent name is whatever you configure.

```sh
claude mcp add --env KENFOLD_AGENT=claude-code --transport stdio --scope user kenfold -- /abs/path/to/bin/kenfold mcp
```

```toml
[mcp_servers.kenfold]            # Codex
command = "/abs/path/to/bin/kenfold"
args = ["mcp"]
env = { KENFOLD_AGENT = "codex" }
```

With `make up-embed`, also set `KENFOLD_EMBED_URL=http://127.0.0.1:11435/v1` for stdio servers.

**ChatGPT** needs a public HTTPS endpoint with OAuth; planned for Phase 4.

## Session hooks (Claude Code, Codex)

MCP tools only run when the model decides to call them. Hooks make memory automatic:

- **Session start**: the hook loads memory for the current repository (pending handoff, recent sessions, project knowledge, preferences) and gives it to the model as context.
- **During the session**: your requests, the final response, and Claude Code's compaction summary are recorded in a local log (`~/.local/state/kenfold`, readable only by you, secrets redacted). This adds no network calls.
- **Session end**: the log becomes a short session summary (an `episodic` memory), so the next agent, in any tool, knows what happened. If Kenfold is down, the summary is kept and sent at the next session start.

Sessions outside a git repository are not recorded. The hook never blocks the agent: problems are reported as a warning.

Set it up per agent. Store the agent's key in a file, then print the hooks configuration:

```sh
make build
mkdir -p ~/.config/kenfold && (umask 077; make -s key AGENT=claude-code > ~/.config/kenfold/claude-code.key)
bin/kenfold hook config claude-code --key-file ~/.config/kenfold/claude-code.key
```

Merge the printed `hooks` object into `~/.claude/settings.json`. For Codex, use a `codex` key and `bin/kenfold hook config codex`, and save the output as `~/.codex/hooks.json`. Codex asks you to review new hooks: run `/hooks` once to trust them.

`--no-capture` loads memory at session start without recording sessions.

## How memory works

- **Projects** are identified by the git remote URL, normalized: `git@github.com:Org/Repo.git` and `https://github.com/org/repo` are the same project. Local paths are rejected, since they differ per checkout. Memories without a project are user-wide and are included everywhere.
- **remember** stores a self-contained statement. Storing the same statement again (ignoring case, spacing, and trailing punctuation) returns the existing memory. With `supersedes`, the old memory is retired and kept as history. When a new memory resembles an existing one, `remember` returns it in `similar` so the agent can supersede or forget whichever is outdated. Nothing is merged automatically.
- **Secrets are rejected.** Writes containing credentials (API keys and tokens from common providers, private keys, passwords in URLs or assignments, JWTs) fail with a message that names the kind of secret without repeating it. `bin/kenfold scan` finds secrets stored before this filter existed; `--redact` removes them.
- **Preferences** written by agents are `proposed` and are not served until you approve them. Approved memories get `trust = user`.
- **handoff** leaves one active note per project (a new one replaces the old). **resume** returns it and records who picked it up; **get_context** only offers handoffs nobody has resumed.
- **recall** fuses full-text and vector rankings (Reciprocal Rank Fusion). If the embedding provider is down, Kenfold falls back to full-text search and embeds missed memories later.

Review what agents stored:

```sh
make build
bin/kenfold memory list --status proposed   # e.g. preferences waiting for approval
bin/kenfold memory approve <id>
bin/kenfold memory forget <id> --reason "outdated"
```

## MCP tools

| Tool | Purpose |
|---|---|
| `get_context` | Load preferences, a pending handoff, recent session summaries, project knowledge, and task-relevant memories at task start |
| `remember` | Store a durable memory (optionally superseding an old one) |
| `recall` | Natural-language search over shared memory |
| `handoff` | Leave a note for the next agent or session |
| `resume` | Pick up the latest handoff |
| `forget` | Soft-delete a memory |

Full contract: [docs/mcp-tools.md](docs/mcp-tools.md).

## Security

- Ports are published on `127.0.0.1` only. Do not expose Kenfold to a network: there is no TLS, and until OAuth (Phase 4) there is no per-user authorization; any valid key can read all memory.
- `/mcp` requires an API key (`KENFOLD_AUTH=apikey`, the default). Only a SHA-256 hash of each key is stored. Revoke a key with `bin/kenfold key revoke <prefix>`.
- `/mcp` also enforces a Host allowlist (DNS rebinding) and rejects cross-site browser requests. `/healthz` and `/readyz` are unauthenticated and expose no data.
- `KENFOLD_AUTH=none` disables authentication; every write is then attributed to the client's self-reported name.
- The secret filter is pattern-based: it catches well-known token formats and random-looking values assigned to secret-named fields, not every possible secret. Treat it as a safety net; agents are still told never to store secrets.

## CLI

```
kenfold serve                        HTTP server (MCP at /mcp, /healthz, /readyz)
kenfold mcp                          MCP over stdio
kenfold migrate [up|down|status]     database migrations
kenfold key create <agent>           create an API key (printed once, to stdout)
kenfold key list [--all]             list keys (--all includes revoked)
kenfold key revoke <id|prefix>       revoke a key
kenfold memory list [--status S] [--type T] [--scope S] [--limit N]
kenfold memory approve <id>          approve a proposed memory
kenfold memory forget <id> [--reason R]
kenfold reindex                      embed memories missing an embedding for the configured model
kenfold scan [--redact]              find (and remove) secrets stored before the secret filter
kenfold hook                         session hook for Claude Code and Codex (event JSON on stdin)
kenfold hook config <claude-code|codex> [--key-file F]
                                     print the hooks configuration for a client
kenfold version
```

## Configuration

| Variable | Default |
|---|---|
| `KENFOLD_HTTP_ADDR` | `127.0.0.1:7077` |
| `KENFOLD_DATABASE_URL` | `postgres://kenfold:kenfold@127.0.0.1:54329/kenfold?sslmode=disable` (compose database) |
| `KENFOLD_AUTO_MIGRATE` | `false` (`true` in compose); `serve` and `mcp` refuse to run on an outdated schema |
| `KENFOLD_ALLOWED_HOSTS` | `localhost,127.0.0.1,::1`, the Host allowlist for `/mcp` |
| `KENFOLD_AUTH` | `apikey`, or `none` |
| `KENFOLD_AGENT` | agent name for `kenfold mcp` (stdio), e.g. `codex` |
| `KENFOLD_EMBED_URL` | unset (full-text search only). Any OpenAI-compatible embeddings API, e.g. `http://127.0.0.1:11434/v1` for Ollama |
| `KENFOLD_EMBED_MODEL` | `bge-m3`. Must produce 1024-dimensional vectors |
| `KENFOLD_EMBED_API_KEY` | unset; sent as a bearer token to the embeddings API |
| `KENFOLD_EMBED_DIMENSIONS` | `false`; send `dimensions=1024` (for models such as `text-embedding-3-large`) |
| `KENFOLD_SEARCH_MAX_DISTANCE` | `0.55`, the cosine distance above which vector matches are ignored |
| `KENFOLD_LOG_LEVEL` | `info` |

The hook reads `KENFOLD_URL` (default `http://127.0.0.1:7077/mcp`), `KENFOLD_API_KEY` (unless `--key-file` is given), and `KENFOLD_STATE_DIR` (default `~/.local/state/kenfold`).

Changing `KENFOLD_EMBED_MODEL` to another 1024-dimensional model needs no migration: vector search only compares memories embedded by the configured model, and the server re-embeds the rest in the background (or run `kenfold reindex`).

## Development

```sh
make test               # unit tests + stdio end-to-end test (no database needed)
make test-integration   # store, API key, migration, cross-agent, and handoff-scenario tests against the compose Postgres
make lint               # gofmt + go vet
make build              # ./bin/kenfold
make logs | make down
```

Layout:

```
cmd/kenfold/          CLI: serve, mcp, hook, admin commands; end-to-end tests
internal/mcpserver/   MCP tool definitions (source of truth for the contract) and handlers
internal/store/       PostgreSQL persistence and hybrid search
internal/hook/        Claude Code / Codex session hook: context injection, capture, spool
internal/secrets/     credential detection and redaction
internal/apikey/      API keys and the bearer-token verifier
internal/embed/       OpenAI-compatible embeddings client
internal/httpserver/  HTTP routing, health probes, Host/CORS protection, auth
internal/memory/      domain types (memory types, trust, status, scopes, agent names)
internal/config/      environment configuration
migrations/           SQL migrations (goose, embedded in the binary)
docs/                 ADRs and specs
```

## Roadmap

| Phase | Scope |
|---|---|
| **0** ✅ | Schema, MCP contract, skeleton, compose |
| **1** ✅ | Real storage and hybrid search, API keys, CLI, Claude Code / Codex / OpenCode setup |
| **2** ✅ | Secret filter, duplicate and contradiction hints, session hooks with automatic session summaries |
| 2b | Model-based extraction and classification of memories from sessions |
| 3 | Graph relations, code indexing, commit-based invalidation, recency and graph ranking, rerank |
| 4 | OAuth 2.1, remote deployment, ChatGPT, object storage |
| 5 | Consolidation, review dashboard, benchmarks |
