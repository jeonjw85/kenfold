# Retrieval eval results

Run with `make eval-search` (needs `make up-embed`). Measured 2026-09-28 on an Apple M5 (4 performance + 6 efficiency cores), CPU in Docker:

- Embeddings: `bge-m3` on Ollama 0.34.4, 4 threads.
- Reranker: `bge-reranker-v2-m3` Q8_0 on llama.cpp server v0.5.0, 4 threads.

Corpus (`corpus.json` + `holdout.json`):

- 110 memories in three projects plus user-wide ones: session summaries, extracted facts linked to them, project rules, code facts, preferences, three superseded memories, and three memories whose code no longer exists.
- 67 queries in Korean and English.
- The 43 **dev** queries were used to tune the pipeline. The 24 **holdout** queries (and 15 of the memories) were written afterwards by an independent author without access to the ranking code, and were run once before this report.

Recall@5 is the share of a query's relevant memories found in the top 5 (over at most 5). Hit@5 is whether any is. MRR@10 is the reciprocal rank of the first relevant memory.

| Configuration | recall@5 dev | recall@5 holdout | hit@5 all | MRR@10 all |
|---|---|---|---|---|
| Full-text only | 0.622 | 0.438 | 0.657 | 0.503 |
| Full-text + signals (`make up`) | 0.645 | 0.438 | 0.672 | 0.514 |
| Hybrid, Phase 2 baseline | 0.845 | 0.875 | 0.925 | 0.795 |
| Hybrid + signals (`make up-embed RERANK=0`) | 0.903 | 0.896 | 0.970 | 0.825 |
| Hybrid + rerank, no graph or signals | 0.909 | 0.938 | 0.970 | 0.924 |
| Full pipeline without graph expansion | 0.944 | 0.938 | 0.985 | 0.934 |
| Full pipeline without recency | 0.921 | 0.958 | 0.970 | 0.934 |
| Full pipeline without staleness | 0.955 | 0.958 | 0.985 | 0.927 |
| **Full pipeline (`make up-embed`)** | **0.955** | **0.958** | **0.985** | **0.934** |

"Signals" are staleness and temporal intent. Graph expansion and the aging of session summaries apply only with a reranker (see below).

- **Target met:** at least 0.90 on dev and 0.85 on holdout. No query, dev or holdout, scores below the hybrid baseline.
- **Latency:** the full pipeline averages 1.8 s per query, mostly the reranker scoring 15 memories. Hybrid search without it takes about 25 ms (including the query embedding), and full-text search a few milliseconds.

## What changed during tuning

- **Graph expansion and summary aging need calibrated scores.** Both are soft adjustments. On first-stage scores, which are fused ranks in a narrow band, they reordered too much: without a reranker, each lowered dev recall@5 from 0.845 to 0.833. They now apply only when the reranker has scored the candidates. Staleness and temporal intent are strong by design and apply either way; together they raise hybrid from 0.845/0.875 to 0.903/0.896.
- **Rerank depth.** 15 candidates gave the same recall as 20 and 30 at 1.5 s instead of 1.7–2.0 s. 10 was worse (0.917).
- **Relevance labels.** The first dev labels listed only the extracted fact for questions that the session summary answers as well. Adding the summaries (23 labels) raised every configuration. It changed no ranking code.

## What the numbers do not show

- **The staleness factors are priors, not fitted values.** The dev set has only two stale memories. On holdout, staleness does not change recall@5 but raises MRR@10 from 0.875 to 0.896 (outdated memories move below current ones).
  - One dev miss remains: for "how do we style components?", the reranker rates the stale CSS-modules memory 20× higher than the current Tailwind one, so even at 5% of its score it still ranks first. It is marked stale to the agent.
  - Hiding stale memories would be wrong: a memory can mention a removed file and still be accurate ("we moved from Button.module.css to Tailwind").
- **Remaining misses.**
  - `dash-login-loop` and `dash-styling` have an answer at rank 6.
  - `pay-webhook-design` has four relevant memories, three of them in the top 5.
  - `pay-orm` loses the "sqlc was considered" memory.
  - `ho-dash-translations` finds the Korean-translation session but not the i18n rule.
- **Temporal intent is a keyword match** ("last time", "지난번", "최근", ...). A rule that happens to contain "latest" gets the recency treatment.
- **Small and synthetic.** The set checks that each stage does what it is meant to. It says nothing about how Kenfold compares to other systems, or about corpora with thousands of memories.
