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
- **Provenance**: every memory records which agent wrote it, when, from what evidence, and how far to trust it
- **No silent overwrites**: contradictions supersede older memories and keep history
- **Handoff** between agents: stop in Claude Code, resume in Codex
- **Poisoning-aware**: external content is never promoted to trusted memory automatically

> **Status: Phase 0 (skeleton).** The server, schema, and MCP tool contract are in place. Tool handlers are stubs that return "not implemented". Not for real use yet.

## Quickstart

Requirements: Docker with Compose. Go 1.27+ for local development.

```sh
make up        # builds the image, starts Postgres 18 + pgvector and Kenfold, applies migrations
curl -s localhost:7077/readyz   # {"status":"ready"}
```

MCP endpoint: `http://127.0.0.1:7077/mcp` (Streamable HTTP, stateless).

> **Security:** there is no authentication yet. Ports are published on `127.0.0.1` only. Do not expose Kenfold to a network until API keys / OAuth land (Phase 1 / Phase 4).

## Connect your agents

**Claude Code**

```sh
claude mcp add --transport http kenfold http://127.0.0.1:7077/mcp
```

**Codex** (`~/.codex/config.toml`)

```toml
[mcp_servers.kenfold]
url = "http://127.0.0.1:7077/mcp"
```

**OpenCode** (`opencode.json`)

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "kenfold": { "type": "remote", "url": "http://127.0.0.1:7077/mcp" }
  }
}
```

**stdio** (clients that spawn a subprocess): build with `make build`, then use `./bin/kenfold mcp` as the command.

**ChatGPT** requires a public HTTPS endpoint with OAuth; planned for Phase 4.

## MCP tools

| Tool | Purpose |
|---|---|
| `get_context` | Load preferences, project knowledge, and pending handoff at task start |
| `remember` | Store a durable memory (optionally superseding an old one) |
| `recall` | Natural-language search over shared memory |
| `handoff` | Leave a note for the next agent/session |
| `resume` | Pick up the latest handoff |
| `forget` | Soft-delete a memory |

Full contract: [docs/mcp-tools.md](docs/mcp-tools.md).

## Development

```sh
make test               # unit tests + stdio end-to-end test
make test-integration   # migration tests against the compose Postgres (uses a throwaway database)
make lint               # gofmt + go vet
make build              # ./bin/kenfold
make logs | make down
```

CLI:

```
kenfold serve                     HTTP server (MCP at /mcp, /healthz, /readyz)
kenfold mcp                       MCP over stdio
kenfold migrate [up|down|status]  database migrations
kenfold version
```

Configuration (environment):

| Variable | Default |
|---|---|
| `KENFOLD_HTTP_ADDR` | `127.0.0.1:7077` |
| `KENFOLD_DATABASE_URL` | `postgres://kenfold:kenfold@127.0.0.1:54329/kenfold?sslmode=disable` (compose DB) |
| `KENFOLD_AUTO_MIGRATE` | `false` (`true` in compose) |
| `KENFOLD_ALLOWED_HOSTS` | `localhost,127.0.0.1,::1`, the Host allowlist for `/mcp` |
| `KENFOLD_LOG_LEVEL` | `info` |

Layout:

```
cmd/kenfold/          CLI entrypoint
internal/mcpserver/   MCP tool definitions (source of truth for the contract)
internal/httpserver/  HTTP routing, health probes, Host/CORS protection
internal/memory/      domain types (memory types, trust, status)
internal/config/      environment configuration
migrations/           SQL migrations (goose, embedded in the binary)
docs/                 ADRs and specs
```

## Roadmap

| Phase | Scope |
|---|---|
| **0** ✅ | Schema, MCP contract, skeleton, compose |
| 1 | Real storage and search, API keys, Claude Code / Codex / OpenCode setup |
| 2 | Write pipeline: extraction, dedup, supersede, secret filter, auto-capture hooks, handoff |
| 3 | Graph relations, code indexing, commit-based invalidation, hybrid retrieval |
| 4 | OAuth 2.1, remote deployment, ChatGPT, object storage |
| 5 | Consolidation, review dashboard, benchmarks |
