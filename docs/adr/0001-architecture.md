# ADR-0001: Kenfold architecture baseline

- Status: accepted
- Date: 2026-09-27

## Context

Every AI tool (Claude Code, Codex, OpenCode, ChatGPT, local models) keeps its own memory, so knowledge learned in one is invisible to the others. Kenfold is an independent memory layer that all of them share over MCP.

Existing projects already cover "shared memory over MCP" (memorix, mcp-memory-service, Engram, and others) and general agent memory (Mem0, Zep/Graphiti, Letta, Cognee). Kenfold differentiates on:

1. **Typed memory with distinct lifecycles**, not one store with tags.
2. **Cross-agent provenance and conflict handling**: who wrote what, when, from which evidence; contradictions supersede, never overwrite; facts carry validity time.
3. **Session handoff** between different agents.
4. **Codebase memory invalidated by code changes** (file hash / commit).
5. **Memory-poisoning defense**: trust levels per origin; memory is served as data, never as instructions.

## Decisions

### Language: Go

The ML-heavy parts (embedding, extraction, reranking) are HTTP calls to model providers, so Python's ecosystem advantage does not apply. Go gives a single static binary (easy `brew install`, 25 MB distroless image), low idle footprint for an always-on server, and an official MCP SDK (`modelcontextprotocol/go-sdk`) that supports the stateless 2026-07-28 protocol revision. The owner's web dashboard (Phase 5) is rendered by the same binary with Go templates and no JavaScript, so there is no second toolchain or build step.

Known cost: tree-sitter (Phase 3 code indexing) requires cgo, which complicates cross-compilation. Resolved in Phase 3 with a pure-Go tree-sitter runtime (see [ADR-0002](0002-retrieval-and-code-refs.md)).

### Storage: PostgreSQL + pgvector as the single source of truth

- One database holds memories, embeddings, graph edges, and temporal data, so a memory and its relations commit in one transaction.
- **No separate graph DB initially.** `memory_edge` plus recursive CTEs cover 1–2 hop traversal. A `GraphStore` interface will allow adding Neo4j/FalkorDB later if traversal depth or volume demands it.
- **No Redis.** Temporary context uses `expires_at`; background jobs will use a Postgres-backed queue (River).
- **Object storage** (local FS → MinIO/S3) for raw transcripts and large evidence, referenced by `memory.evidence_uri`. Not in Phase 0.
- PostgreSQL 18+ is required (`uuidv7()` for time-ordered IDs).

### Memory model

One `memory` table with a `type` discriminator and type-specific `attrs jsonb`, rather than a table per type. Retrieval rules differ per type, the storage shape does not.

| Type | Write rule | Lifetime | Retrieval |
|---|---|---|---|
| semantic | extracted/consolidated from episodes | long; superseded on conflict | vector + graph |
| episodic | auto at session end | decays; summarized when old | time + vector |
| project | explicit or extracted | until project archived | scope filter, injected |
| preference | requires user confirmation | permanent, versioned | always injected (small) |
| codebase | indexer + agents | invalidated on hash/commit change | path/symbol + vector + deps |
| temporary | free | TTL (hours–days); may be promoted | session/project key |

Key columns: `scope` (`user` \| `project:<id>` \| `repo:<id>`), provenance (`source_agent`, `source_session`, `evidence_uri`, `trust`, `confidence`), lifecycle (`status`, `supersedes`), and time (`valid_from`/`valid_to` = world validity, `expires_at` = TTL).

Constraints enforce the invariants that must never be violated by any writer: valid enum values, temporary memories must have a TTL, confidence in [0,1], validity ranges ordered, no self-referencing edges.

**Embedding dimension is fixed at 1024** (default model: `bge-m3`, multilingual; Korean and English mix in practice). `memory.embedding_model` records which model produced each vector, and search only compares vectors from the configured model, so switching to another 1024-dimensional model needs no migration: memories are re-embedded in the background. A different dimension requires a migration.

**Project identity** is the normalized git remote URL, so the same repo maps to the same project across agents and machines.

### Interface: MCP first, six tools

`get_context`, `remember`, `recall`, `handoff`, `resume`, `forget`. Few tools with clear descriptions work better with agents than many fine-grained ones. See [docs/mcp-tools.md](../mcp-tools.md).

- **Transports**: stdio (`kenfold mcp`) for subprocess-style clients; Streamable HTTP (`kenfold serve`, `/mcp`) for everything else.
- **Stateless HTTP** from day one: any replica can serve any request, and it is required for the 2026-07-28 protocol. Clients on 2025-11-25 still work.
- Tool annotations mark read-only vs. destructive tools so clients can auto-approve reads.
- Server `instructions` stay under 512 characters (Codex guarantees only that much is used).

### Security posture

- Kenfold listens on loopback by default; compose publishes ports on `127.0.0.1` only. Since Phase 4 it can be reached remotely through a tunnel or TLS proxy with `KENFOLD_PUBLIC_URL` (see [ADR-0003](0003-remote-access-and-oauth.md)).
- `/mcp` enforces a Host allowlist (DNS-rebinding defense) and rejects cross-origin browser requests. The SDK's built-in rebinding check only applies to connections arriving on a loopback address, which is not the case behind Docker port publishing, so Kenfold enforces its own.
- Auth: API keys since Phase 1 (one per agent; only a SHA-256 hash is stored; the key, not the client's self-reported name, determines `source_agent`). Since Phase 4, a built-in OAuth 2.1 authorization server for remote clients such as ChatGPT: the owner approves each client on a password-protected consent page, read-only or read and write, and names the agent its writes are attributed to.
- Secret filter on writes since Phase 2: `remember` and `handoff` reject content with credentials, hooks redact captured text locally before it is written to disk, and `kenfold scan --redact` cleans memories stored earlier. Memory is rendered to agents as data, and `trust = external` content is never auto-promoted.
- The owner's dashboard (`/dashboard/`, Phase 5) answers only for loopback host names and refuses requests that carry proxy forwarding headers, so a public URL or a tunnel does not expose it unless `KENFOLD_DASHBOARD=remote`. Login uses the owner password with the consent page's lockout. Sessions are held in memory (2 hours idle, 12 hours at most) and end when the password changes. Every change is a POST with a CSRF token and cross-origin protection, under a content security policy that allows no scripts. New API keys are still created only on the server.

### Defaults chosen

- Personal, local-first deployment. Multi-user/team tenancy is out of scope until after Phase 4; `scope` leaves room for it.
- Local embeddings (Ollama + bge-m3) by default so code and conversations stay on the machine; API providers are optional.
- Memory extraction uses a small local chat model (`qwen3.5:4b` through Ollama's OpenAI-compatible API) so session content stays on the machine. The model only proposes: validation is mechanical (allowed categories, evidence grounded in the session and outside pasted text, no secrets, novelty), and extracted memories are reviewed before they are served, because extraction is the main path by which injected instructions could become trusted memory.

## Roadmap

| Phase | Scope | Done when |
|---|---|---|
| 0 | Schema, MCP contract, repo skeleton, compose | `docker compose up` boots; MCP `tools/list` works |
| 1 | Real storage/search for all types (full-text + vector, RRF), API keys, CLI, client setup for Claude Code / Codex / OpenCode, handoff/resume, exact dedup, supersede | a decision remembered in Claude Code is recalled in Codex (E2E test) |
| 2 | Secret filter on writes, normalized dedup, similarity/contradiction hints, hook-based auto-capture of session summaries (Claude Code, Codex), handoff | handoff scenario E2E passes |
| 2b | Model-based extraction of memories from session summaries (proposed for review by default) and type classification, with a local chat model (default `qwen3.5:4b`) | extracted memories measured on an internal eval set (dev + holdout; see `internal/extract/testdata/RESULTS.md`) |
| 3 | Graph expansion, recency and staleness signals, rerank, code references with commit-based invalidation (tree-sitter symbols), REST API; see [ADR-0002](0002-retrieval-and-code-refs.md) | recall@5 ≥ 0.90 dev / ≥ 0.85 holdout on the internal eval set, never below the hybrid baseline |
| 4 | OAuth 2.1 authorization server, remote deployment behind a tunnel or TLS proxy; object storage and an OpenAI-compatible proxy deferred (see [ADR-0003](0003-remote-access-and-oauth.md)) | recall works from ChatGPT. Verified with the MCP SDK's OAuth client over HTTPS and against ChatGPT's published client metadata; a real ChatGPT connection awaits a public deployment |
| 5 | Export/import (done: JSON Lines archive without embeddings or credentials), review dashboard (done: server-rendered, local-only by default), consolidation (done: proposals the owner applies; see [ADR-0004](0004-consolidation.md)), LongMemEval/LoCoMo evals | ongoing |

## Consequences

- Postgres is a hard dependency even for a single user. Accepted: it is what makes transactions over memory + graph + vectors possible. An embedded mode (e.g. SQLite) may be considered later for zero-setup installs.
- A fixed embedding dimension makes changing to a model of a different size a deliberate migration; same-size model changes only re-embed.
- MCP is the primary contract. A REST API (`/api/v1`, since Phase 3) serves non-MCP clients with the same protections and keys. The dashboard does not go through it: it runs in the same process and authenticates the owner by password, not by API key.
