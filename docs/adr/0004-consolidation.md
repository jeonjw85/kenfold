# ADR-0004: Consolidation

- Status: accepted
- Date: 2026-09-28

## Context

Memory accumulates duplicates and contradictions. Several agents store the same rule in their own words, and a decision that changed leaves both versions active. Session summaries pile up too: one per session, most of them old. All of this makes search and the context given at session start worse. ADR-0001 plans for this: semantic memories are "superseded on conflict", and episodic memories are "summarized when old".

Consolidation changes what agents are told, which is why memory poisoning matters here. A wrong merge or an instruction hidden in a session summary would become trusted memory. So would a hallucinated detail in a summary.

## Decision

**Kenfold proposes, the owner decides.** A background worker (on when a chat model is configured; `KENFOLD_CONSOLIDATE`) records proposals. Nothing changes until the owner applies one in the dashboard or with `kenfold consolidate apply`. There is no automatic mode.

Three kinds of proposal:

| Kind | Found by | Applying it |
|---|---|---|
| duplicate | a similar pair (same scope and type) that the model judges "one says everything the other says" | retires the other; the one that says more stays |
| conflict | a similar pair that the model judges contradictory | retires the older; the newer stays, since a contradiction usually means a decision changed |
| digest | at least 6 session summaries older than 30 days in a scope | stores a digest of the oldest 10 as a new episodic memory and retires them |

- **Nothing is rewritten.** Duplicates and conflicts keep one of the existing memories as it is, with its author, date, and trust. The model never writes a merged statement, so it cannot add or drop a detail, and provenance is kept. Retired memories become `superseded` (history). A `replaces` edge links them from the memory that stays.
- **Candidates are found mechanically**, like similarity hints: cosine distance ≤ 0.25 between embeddings, or trigram similarity ≥ 0.5. Only settled memories (older than an hour) are compared. Episodic and temporary memories are left out, and so is any memory in a pending proposal.
- **Two readings.** A pair judged a duplicate or conflict is asked again with the two memories swapped, and it is proposed only if both answers agree, on which memory to keep as well. A small model favors one position, and a wrong proposal retires a memory that is still needed. On the internal eval set this removed the only such error (see `internal/consolidate/testdata/RESULTS.md`).
- **Digests are checked against their sources.** Every file name, identifier, path, and number in a digest must appear in the sessions it summarizes. The digest must also be free of secrets. Otherwise it is dismissed and those sessions are left as they are. The prompt marks session text as data, and closing tags inside it are neutralized. A digest takes the date of the last session it covers, so it does not count as a recent session. The extractor skips digests, since their sessions were extracted already; this holds after an import too, because it looks at the memory itself, not at an extraction job.
- **Each set of memories is judged once.** A proposal's member ids are unique. A pair the model found distinct is recorded as dismissed, and a proposal the owner rejects is not made again. A proposal whose members changed before the owner decided (forgotten, superseded, expired) becomes stale and cannot be applied.
- **The model is shared.** The worker runs every 15 minutes, judges at most 4 pairs and writes at most one digest per run, and skips a run while session summaries wait for extraction.

## Consequences

- The owner has one more queue to review. Pending proposals are counted with proposed memories in the dashboard's Review badge.
- Consolidation needs a chat model. Without one, the dashboard does not show the page and nothing is proposed.
- A pair judged distinct is never judged again, even after a better model is configured. A later version could re-judge dismissed pairs when the model changes.
- Proposals are not part of `kenfold export`. After an import, rejected pairs may be proposed again.
- Consolidation calls add load to the model. `remember` calls that classify a type then wait longer and may fall back to the default type.
