# ADR-0002: Retrieval pipeline and code references

- Status: accepted
- Date: 2026-09-28

## Context

Phase 3 of [ADR-0001](0001-architecture.md) asked for graph and recency signals in ranking, reranking, and codebase memory that is invalidated when code changes. It was to be done when an internal eval set reaches a recall@5 target.

By Phase 2b, search was hybrid (full-text and vectors fused with RRF). The weak spots were:

- Memories phrased unlike the query.
- Questions about recent work.
- Facts split between a session summary and the memories extracted from it.
- Memories about code that no longer exists, which were served as if still true.

## Decisions

### Measure first, on a corpus with a holdout

`internal/retrieve/testdata/corpus.json` has 95 memories in three projects plus user-wide ones, and 43 dev queries in Korean and English. `holdout.json` adds 15 memories and 24 queries. An independent author wrote the holdout without access to the ranking code, after the pipeline was tuned on dev.

Result: recall@5 of 0.955 on dev and 0.958 on holdout for the full pipeline, versus 0.845 and 0.875 for the Phase 2 hybrid baseline. No query scores below the baseline. `make eval-search` measures every configuration with a real embedding model and reranker; results are in `internal/retrieve/testdata/RESULTS.md`.

Target: full pipeline recall@5 ≥ 0.90 on dev and ≥ 0.85 on holdout, and never below the hybrid baseline. The corpus is small and synthetic: it checks that each stage does what it is meant to, not how Kenfold compares with other systems.

### A staged pipeline in one place

`internal/retrieve` owns ranking; `recall` and `get_context` call it. The stages:

1. Hybrid first stage: a pool of 30.
2. Graph expansion from the 8 best candidates (edges in both directions, plus siblings derived from the same source).
3. Optional cross-encoder rerank of the best 15.
4. Recency and staleness multipliers.

Each stage degrades independently: no embedder means full-text only, and a failed or slow reranker (4 s) keeps the first-stage order. Constants are named in the code with their rationale. The dev set tuned only the pool and rerank sizes; the staleness factors are stated priors, since the dev set has two stale memories.

Graph expansion and the aging of session summaries apply only when the reranker has scored the candidates. They are soft adjustments sized for its probabilities. First-stage scores are fused ranks packed into a narrow band, and there each adjustment lowered dev recall@5 from 0.845 to 0.833. Staleness and temporal intent are strong by design and apply either way.

Recency is not a blanket decay. Facts do not become less true with age, so only session summaries decay without temporal intent (at most 40%, half-life 30 days). A query about recent work ("last time", "지난번", "최근") decays everything with a 7-day half-life. The intent detector is a bilingual regular expression, which can misfire on rules that happen to contain "last" or "latest".

### Reranker: llama.cpp with bge-reranker-v2-m3

Ollama has no rerank API, so the compose file adds a `reranker` service: llama.cpp's `llama-server --reranking` serving `bge-reranker-v2-m3` (Q8_0, 636 MB, multilingual like bge-m3). The client speaks the `/rerank` shape shared by llama-server, Jina, Cohere v2, and Voyage, so a hosted reranker is a configuration change. Raw logits (llama-server) are mapped through a sigmoid; that keeps the order and makes scores comparable across queries.

On the M5, 15 documents take about 1.2–1.6 s with 4 threads. Q4_K_M was slower than Q8_0 on this CPU, and 10 threads were slower than 4 or 6. Reranking is on by default in `make up-embed` because it gave the largest single gain. `RERANK=0` removes it for machines where the latency matters more.

### Code references: the server records, clients verify

The server never sees repositories; agents and hooks run next to them. So:

- **On write**, the server extracts mentioned files and symbols from `project`, `codebase`, and `semantic` memories in a project scope (`memory_ref`, migration 00005). Extraction favors precision:
  - Paths need a known extension and cannot be URLs, absolute paths, or product names.
  - Symbols need backticks or call syntax, or must look like code when exactly one file is mentioned.
- **In the repository**, `kenfold hook` (at session start, within a 2 s budget) and `kenfold refs sync` fetch the project's references over a small REST API (`/api/v1/refs`). They hash each one at `HEAD`: a file's blob id, or a whitespace-insensitive hash of the symbol's definitions located with tree-sitter. Then they report back.
- **States** are `pending`, `current`, `changed`, `missing`, `unresolved`. A memory is stale when a referenced file or symbol is missing or a referenced symbol changed. A changed file alone is too common to mean anything. Stale memories are:
  - multiplied by 0.05 (missing) or 0.6 (changed symbol) in ranking;
  - sorted last in `get_context`'s project knowledge;
  - marked in the hook's context;
  - listed by `kenfold refs status` and `kenfold memory list --stale`.

Two rules keep commit-based checks honest:

- **Anchor only committed code.** A reference whose file has uncommitted changes waits, because the memory may describe the uncommitted version.
- **Compare only descendants.** An anchored reference is checked only where `HEAD` contains the anchor commit. An older clone or another branch says nothing about it, rather than reporting history as a change.

Anchoring the first time a client sees a reference (not at the memory's creation time) means a memory written about code that had already changed is anchored to the new code. The server cannot know the commit a memory was written against. Anchoring at first sight is the conservative default: it can miss staleness that happened before the first sync, but it never invents it.

### tree-sitter without cgo

ADR-0001 flagged that tree-sitter needs cgo. [gotreesitter](https://github.com/odvcencio/gotreesitter) (MIT) is a pure-Go runtime that loads the upstream parse tables, so Kenfold stays a static, cross-compiled binary. It is used only to find definitions. The rule is language-generic: an identifier in the `name` field of a node that is not a call, reference, argument, or JSX element, plus a few assignment and declarator shapes. Tests cover Go, TypeScript/TSX, Python, Rust, Java, and C. Parsing runs with a timeout, a size limit, and panic recovery, since the runtime is young.

Binary sizes:

- `make build` embeds 16 grammars; with the runtime they add about 10 MB (13.8 → 23.7 MB).
- Plain `go build` embeds all of them (38 MB).
- The server image embeds none, because the server never parses code. The linked runtime still adds about 6 MB (19.4 MB binary).

Symbols in unsupported languages are skipped rather than guessed; their files are still tracked.

## Consequences

- `make up-embed` now downloads about 1.9 GB and runs two model servers. Search latency with the reranker is 1–2 s on CPU, versus about 25 ms without it.
- Staleness is only as fresh as the last sync. Code changed on a machine that never runs the hook or `refs sync` is not noticed.
- References to code in other repositories, generated code, or unsupported languages stay `unresolved` or file-level.
- The REST API is the first non-MCP surface. It shares the MCP endpoint's protections and API keys, and is where a future dashboard will attach.
- The eval corpus is part of the repository. Changes to ranking should be measured with `make eval-search` before and after.
