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

> **Status: Phase 4.** Shared storage, hybrid search with **reranking, graph expansion, and recency**, API keys, setup for Claude Code, Codex, and OpenCode, a server-side **secret filter**, duplicate and contradiction hints, **session hooks** that load memory at session start and record a summary at session end, optional **model-based extraction** that turns session summaries into memories for your review, **code references** that flag memories whose code changed or disappeared, and **remote access** with a built-in OAuth server for ChatGPT, claude.ai, and agents on other machines.

## Quickstart

Requirements: Docker with Compose. Go 1.27+ only for development.

```sh
make up                        # Postgres 18 + pgvector and Kenfold on http://127.0.0.1:7077
make key AGENT=claude-code     # prints an API key for Claude Code (shown once)
make key AGENT=codex           # one key per agent, so every memory is attributed correctly
```

`make up` gives full-text search. For better search, which also finds memories phrased differently from the query, run the local search models instead:

```sh
make up-embed                  # adds Ollama with bge-m3 and a llama.cpp reranker (first run downloads ~1.9 GB)
```

Memories written without embeddings are embedded automatically once a model is available. The reranker reads the query and each candidate memory together; on the internal eval set it raised recall@5 from 0.90 to 0.96 (see [Search quality](#search-quality)), at the cost of 1–2 seconds per search on CPU. `make up-embed RERANK=0` leaves it out.

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

**ChatGPT, claude.ai, and other machines** need a public HTTPS URL, behind a tunnel or a reverse proxy, with `KENFOLD_PUBLIC_URL` set:

```sh
KENFOLD_PUBLIC_URL=https://kenfold.example.com make up
docker compose exec kenfold /usr/local/bin/kenfold oauth password   # asked for when you approve a client
```

This turns on Kenfold's OAuth 2.1 server. ChatGPT and claude.ai find it on their own, and you approve each on a consent page, read-only or read and write. Agents with API keys use the public URL as before. See [docs/deploy.md](docs/deploy.md) for Tailscale Funnel, Cloudflare Tunnel, and Caddy, and read its checklist before exposing your memory to the internet.

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

`--no-capture` loads memory at session start without recording sessions. The hook also checks the code that memories refer to at session start (see [Code references](#code-references)); `--no-refs` turns that off.

## Memory extraction (optional)

Session summaries record what happened. With a chat model configured, Kenfold also reads each summary and proposes the durable memories in it: project rules ("use pnpm, not npm"), facts about the code ("the webhook dedupes by event_id"), and your preferences ("answer in Korean"). It skips one-off tasks and anything from pasted documents.

```sh
make up-extract                # up-embed plus qwen3.5:4b (first run downloads ~5.2 GB)
make review                    # go through proposed memories: approve, reject, or replace an old one
```

Extracted memories are `proposed`: nothing is served to agents until you approve it. Each one shows the quote it came from and any existing memory it resembles; approving with "replace" retires the old one. A memory you reject is not proposed again. The same model also picks the type of memories that agents store without one; this adds about 6 seconds to such a `remember` call on CPU, and if the model takes longer than 15 seconds (while it loads, or is busy extracting) the default type is used.

The model runs on CPU in Docker and takes 10–30 seconds per session, in the background. On CPUs with performance and efficiency cores, set `THREADS` to the number of performance cores (default 4; `make up-embed THREADS=6`): Ollama's default of one thread per core was 30–80× slower on an Apple M5, for embeddings as well as chat. Any OpenAI-compatible chat API works (`KENFOLD_CHAT_URL`, `KENFOLD_CHAT_MODEL`). Quality on the internal eval set is recorded in [internal/extract/testdata/RESULTS.md](internal/extract/testdata/RESULTS.md) (`make eval`).

`KENFOLD_EXTRACT_POLICY=auto` activates confident extractions that resemble no existing memory without review. Preferences are always reviewed. Keep the default unless you trust every source of your sessions: extraction is where instructions hidden in pasted content could become memory, and review is the main defense.

## Code references

Memories about code go stale when the code changes. When an agent stores "`RequireAPIKey` in internal/auth/middleware.go hashes the bearer token", Kenfold records the file and the symbol. At session start, the hook hashes them at your repository's `HEAD` (symbols are located with tree-sitter) and tells the server; after a later commit removes or rewrites `RequireAPIKey`, the memory is marked **stale**. Stale memories rank lower in search, come last in the project knowledge given at session start, and are shown to agents with a note such as "(outdated? RequireAPIKey in internal/auth/middleware.go changed since this was written)".

Checks are commit-based: only committed code counts, a reference is anchored only when its file has no uncommitted changes, and a checkout that does not contain the anchoring commit (an older clone, another branch) never reports a change. A changed file alone does not make a memory stale; a changed or removed symbol, or a removed file, does.

```sh
bin/kenfold refs sync                        # check the current repository now (uses KENFOLD_API_KEY or --key-file)
bin/kenfold refs status                      # counts by state, and the memories that may be outdated
bin/kenfold memory list --stale              # the same memories, as a list
```

To check after every commit instead of at session start, add a git hook, e.g. `.git/hooks/post-commit`:

```sh
#!/bin/sh
/abs/path/to/bin/kenfold refs sync --quiet --key-file ~/.config/kenfold/claude-code.key || true
```

Symbols are found in Go, TypeScript/TSX, JavaScript, Python, Rust, Java, Kotlin, Ruby, C#, PHP, C, C++, Swift, Scala, and Bash files with `make build` (`GRAMMARS=...` changes the set); other files are tracked as whole files.

## How memory works

- **Projects** are identified by the git remote URL, normalized: `git@github.com:Org/Repo.git` and `https://github.com/org/repo` are the same project. Local paths are rejected, since they differ per checkout. Memories without a project are user-wide and are included everywhere.
- **remember** stores a self-contained statement. Storing the same statement again (ignoring case, spacing, and trailing punctuation) returns the existing memory. With `supersedes`, the old memory is retired and kept as history. When a new memory resembles an existing one, `remember` returns it in `similar` so the agent can supersede or forget whichever is outdated. Nothing is merged automatically.
- **Secrets are rejected.** Writes containing credentials (API keys and tokens from common providers, private keys, passwords in URLs or assignments, JWTs) fail with a message that names the kind of secret without repeating it. `bin/kenfold scan` finds secrets stored before this filter existed; `--redact` removes them.
- **Preferences** written by agents are `proposed` and are not served until you approve them. Approved memories get `trust = user`.
- **handoff** leaves one active note per project (a new one replaces the old). **resume** returns it and records who picked it up; **get_context** only offers handoffs nobody has resumed.
- **recall** fuses full-text and vector rankings (Reciprocal Rank Fusion), adds memories linked to the best matches (a session summary and the facts extracted from it), reranks the best candidates with a cross-encoder when one is configured, and prefers recent session summaries and memories whose code still exists. Asking about recent work ("what did we do last time", "지난번에") favors recent memories. If a model is down, Kenfold falls back to the stages that still work.

Review what agents stored:

```sh
make build
bin/kenfold memory review                   # interactive: approve, reject, or replace
bin/kenfold memory list --status proposed   # e.g. preferences and extracted memories waiting for review
bin/kenfold memory approve <id>
bin/kenfold memory forget <id> --reason "outdated"
```

## Backup and moving to another server

```sh
docker compose exec -T kenfold /usr/local/bin/kenfold export > kenfold-backup.jsonl   # everything, including history
bin/kenfold export --out backup.jsonl --scope github.com/org/repo --active            # one project, current memories only
bin/kenfold import --dry-run backup.jsonl && bin/kenfold import backup.jsonl
```

An archive holds memories with their history, provenance, and timestamps, plus the links between them and their code references. It leaves out embeddings (the target re-embeds with its own model), API keys, and OAuth clients. Import keeps memories that already exist, so running it twice is harmless. It refuses the whole archive if it is incomplete or if any memory contains a secret. `--out` files are created readable by you only. The archive holds your memory in plain text: store it like the database.

## Search quality

Measured with `make eval-search` on an internal corpus of 110 memories in three projects and 67 queries in Korean and English (details in [internal/retrieve/testdata/RESULTS.md](internal/retrieve/testdata/RESULTS.md)). Recall@5 is the share of the memories that answer a query found in the top 5.

| Setup | Dev (43 queries) | Holdout (24) | Time per search |
|---|---|---|---|
| `make up` (full-text) | 0.65 | 0.44 | a few ms |
| Phase 2 hybrid search | 0.85 | 0.88 | ~25 ms |
| `make up-embed RERANK=0` | 0.90 | 0.90 | ~25 ms |
| `make up-embed` (with reranker) | **0.96** | **0.96** | ~1.8 s |

Times are the search inside Kenfold on an Apple M5 CPU, with the models warm.

The dev queries were used to tune the pipeline; the holdout queries were written afterwards by an independent author without access to the ranking code. This is a small, synthetic set: it shows the pipeline works as intended, not how it compares to other systems.

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

- Ports are published on `127.0.0.1` only. Kenfold has no TLS of its own: expose it only through a tunnel or a TLS reverse proxy, with `KENFOLD_PUBLIC_URL` set (see [docs/deploy.md](docs/deploy.md)).
- `/mcp` requires an API key (`KENFOLD_AUTH=apikey`, the default) or an OAuth access token. Only SHA-256 hashes of keys and tokens are stored. Revoke a key with `bin/kenfold key revoke <prefix>` and an OAuth client with `kenfold oauth revoke <grant>`.
- Every key and approved OAuth client can read all memory (Kenfold has one owner). OAuth clients can be limited to read-only, and the consent page preselects read-only. It is protected by the owner password (Argon2id, locked for 15 minutes after five wrong attempts).
- `/mcp` and the REST API (`/api/v1`, used by `kenfold refs sync` and the hook) enforce a Host allowlist (DNS rebinding) and reject cross-site browser requests. `/healthz` and `/readyz` are unauthenticated and expose no data.
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
kenfold memory list [--status S] [--type T] [--scope S] [--stale] [--limit N]
kenfold memory review [--scope S]    go through proposed memories interactively
kenfold memory approve <id>... [--replaces ID]
kenfold memory reject <id>... [--reason R]
kenfold memory forget <id> [--reason R]
kenfold extract status               extraction progress
kenfold extract run [--limit N]      extract memories from session summaries now
kenfold oauth password               set the owner password for the OAuth consent page
kenfold oauth clients [--all]        approved OAuth clients (ChatGPT, claude.ai, ...)
kenfold oauth revoke <grant>         revoke an OAuth client and its tokens
kenfold refs sync [--dir D] [--quiet]
                                     check the code memories refer to against the repository's HEAD
kenfold refs status [--scope S]      code reference states and memories that may be outdated
kenfold export [--out F] [--scope S] [--active]
                                     write memories, their links, and code references to a JSON Lines archive
kenfold import [--dry-run] <F|->     import an archive (existing memories are kept)
kenfold reindex                      embed memories missing an embedding for the configured model
kenfold scan [--redact]              find (and remove) secrets stored before the secret filter
kenfold hook [--no-capture] [--no-refs]
                                     session hook for Claude Code and Codex (event JSON on stdin)
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
| `KENFOLD_PUBLIC_URL` | unset. The https URL remote clients reach Kenfold at, e.g. `https://kenfold.example.com` (no path); its host is added to `KENFOLD_ALLOWED_HOSTS`, and it enables OAuth |
| `KENFOLD_OAUTH` | on when `KENFOLD_PUBLIC_URL` is set; the built-in OAuth 2.1 authorization server |
| `KENFOLD_OAUTH_DCR` | `true`; dynamic client registration (clients identified by a metadata document URL work either way) |
| `KENFOLD_AGENT` | agent name for `kenfold mcp` (stdio), e.g. `codex` |
| `KENFOLD_EMBED_URL` | unset (full-text search only). Any OpenAI-compatible embeddings API, e.g. `http://127.0.0.1:11434/v1` for Ollama |
| `KENFOLD_EMBED_MODEL` | `bge-m3`. Must produce 1024-dimensional vectors |
| `KENFOLD_EMBED_NAME` | unset; the model name recorded with vectors when `KENFOLD_EMBED_MODEL` is a local alias with identical output (`make up-embed` uses `kenfold-embed`, recorded as `bge-m3`) |
| `KENFOLD_EMBED_API_KEY` | unset; sent as a bearer token to the embeddings API |
| `KENFOLD_EMBED_DIMENSIONS` | `false`; send `dimensions=1024` (for models such as `text-embedding-3-large`) |
| `KENFOLD_SEARCH_MAX_DISTANCE` | `0.55`, the cosine distance above which vector matches are ignored |
| `KENFOLD_RERANK_URL` | unset (no reranking). A `/rerank` API: llama.cpp `llama-server --reranking` (`make up-embed` runs one), Jina, Cohere (`https://api.cohere.com/v2`), Voyage |
| `KENFOLD_RERANK_MODEL` | `bge-reranker-v2-m3` |
| `KENFOLD_RERANK_API_KEY` | unset; sent as a bearer token to the rerank API |
| `KENFOLD_CHAT_URL` | unset (no extraction). Any OpenAI-compatible chat API, e.g. `http://127.0.0.1:11434/v1` for Ollama |
| `KENFOLD_CHAT_MODEL` | `qwen3.5:4b` (`make up-extract` uses `kenfold-extract`, the same model with a thread limit) |
| `KENFOLD_CHAT_API_KEY` | unset; sent as a bearer token to the chat API |
| `KENFOLD_CHAT_REASONING` | `none`, sent as `reasoning_effort`; `omit` for servers that reject the field |
| `KENFOLD_EXTRACT` | on when a chat model is configured |
| `KENFOLD_EXTRACT_POLICY` | `propose` (review everything) or `auto` |
| `KENFOLD_CLASSIFY` | on when a chat model is configured; types memories stored without one |
| `KENFOLD_LOG_LEVEL` | `info` |

The hook and `kenfold refs sync` read `KENFOLD_URL` (default `http://127.0.0.1:7077/mcp`; the REST API is at `/api/v1` next to it) and `KENFOLD_API_KEY` (unless `--key-file` is given); the hook also reads `KENFOLD_STATE_DIR` (default `~/.local/state/kenfold`).

Changing `KENFOLD_EMBED_MODEL` to another 1024-dimensional model needs no migration: vector search only compares memories embedded by the configured model, and the server re-embeds the rest in the background (or run `kenfold reindex`).

## Development

```sh
make test               # unit tests + stdio end-to-end test (no database needed)
make test-integration   # store, API key, migration, cross-agent, handoff, code-reference, and OAuth tests against the compose Postgres
make lint               # gofmt + go vet
make eval               # extraction quality against a real model (needs make up-extract)
make eval-search        # retrieval quality per pipeline stage (needs make up-embed)
make build              # ./bin/kenfold
make logs | make down
```

Layout:

```
cmd/kenfold/          CLI: serve, mcp, hook, admin commands; end-to-end tests
internal/mcpserver/   MCP tool definitions (source of truth for the contract) and handlers
internal/store/       PostgreSQL persistence, hybrid search, graph neighbors, code references
internal/retrieve/    search pipeline (first stage, graph expansion, rerank, recency, staleness); eval corpus
internal/rerank/      rerank API client (llama.cpp, Jina, Cohere, Voyage)
internal/coderef/     code references: extraction from text, git + tree-sitter checks, sync client
internal/restapi/     REST API (/api/v1) for non-agent clients
internal/hook/        Claude Code / Codex session hook: context injection, capture, spool
internal/extract/     model-based memory extraction and classification; eval set
internal/chat/        OpenAI-compatible chat client (structured output)
internal/secrets/     credential detection and redaction
internal/apikey/      API keys and the bearer-token verifier
internal/oauth/       OAuth 2.1 authorization server: metadata, consent, tokens, client metadata documents
internal/authz/       access levels (read, write) shared by the tools and the API
internal/embed/       OpenAI-compatible embeddings client
internal/httpserver/  HTTP routing, health probes, Host/CORS protection, auth for /mcp and /api
internal/memory/      domain types (memory types, trust, status, scopes, agent names)
internal/config/      environment configuration
migrations/           SQL migrations (goose, embedded in the binary)
deploy/               reverse proxy example (Caddy) for remote access
docs/                 ADRs and specs
```

## Roadmap

| Phase | Scope |
|---|---|
| **0** ✅ | Schema, MCP contract, skeleton, compose |
| **1** ✅ | Real storage and hybrid search, API keys, CLI, Claude Code / Codex / OpenCode setup |
| **2** ✅ | Secret filter, duplicate and contradiction hints, session hooks with automatic session summaries |
| **2b** ✅ | Model-based extraction of memories from sessions (reviewed), type classification |
| **3** ✅ | Rerank, graph expansion, recency and staleness in ranking, code references with commit-based invalidation (tree-sitter symbols), REST API |
| **4** ✅ | OAuth 2.1 authorization server (client metadata documents, dynamic registration, `private_key_jwt`, read-only grants), remote deployment behind a tunnel or proxy; verified end to end with the MCP SDK's OAuth client over HTTPS, not yet from ChatGPT itself. Object storage and an OpenAI-compatible proxy were deferred ([ADR-0003](docs/adr/0003-remote-access-and-oauth.md)) |
| 5 | Export/import ✅; consolidation, review dashboard, public benchmarks (LongMemEval, LoCoMo) |
