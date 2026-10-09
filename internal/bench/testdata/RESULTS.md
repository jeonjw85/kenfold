# Public benchmarks — PARTIAL / incomplete validation

## Latest authorized scored retry — partial raw-LoCoMo results

The authorized retry ended at **2026-10-08T00:59:29Z**, exit status **1**.
Unlike the earlier attempt below, its completed answers, judge verdicts, F1 and
evidence recall were preserved per question and independently verified.
**Full public benchmark validation remains incomplete.** These are results for
the first **1,402 of 1,540 raw-LoCoMo questions in dataset order**, not a random
or stratified sample, the complete benchmark, or a comparison with other systems.

| Evaluation | Selected questions | Completed answer/judge pairs | Quality results |
|---|---:|---:|---|
| LoCoMo categories 1–4, raw-turn memories | 1,540 | 1,402 | Partial results below |
| LoCoMo categories 1–4, extracted memories | 1,540 | 0 | Not scored |
| LongMemEval_S, stratified subset | 60 | 0 | Not scored |

### Verified completed prefix

| Category | Scored | Judge correct | Judge accuracy | F1 | Evidence recall@5 | Evidence recall@10 |
|---|---:|---:|---:|---:|---:|---:|
| All | 1,402 | 526 | 37.52% | 0.312 | 0.640 | 0.671 |
| 1 multi-hop | 258 | 56 | 21.71% | 0.180 | 0.374 | 0.410 |
| 2 temporal | 298 | 46 | 15.44% | 0.296 | 0.783 | 0.802 |
| 3 open-domain | 92 | 13 | 14.13% | 0.099 | 0.372 | 0.408 |
| 4 single-hop | 754 | 411 | 54.51% | 0.389 | 0.707 | 0.740 |

F1 is normalized token overlap **without Porter stemming**, not the paper's
published F1. Evidence recall averages only questions with evidence: **1,400**
overall, including 90 of 92 open-domain questions. Zero per-question errors in
the saved table describe completed pairs only; the run itself failed and one
unfinished judge call is excluded from every quality denominator.

### Why the run stopped

The next question, `conv-50-20`, has a saved reader answer but a
`judge-in-flight` checkpoint with no saved verdict. The API usage could not be
confirmed, so the durable budget latched `usage_unavailable` and stopped further
calls. **The $5 allowance was not exhausted.** The retained evidence does not
establish whether the underlying cause was transport failure or a response
without valid usage; it must not be reported as a specific vendor/network error.

The completed 1,402 pairs were preserved; 138 raw questions remain unscored,
including that unfinished judge call. Extracted-LoCoMo scoring and LongMemEval
ingestion/QA were not reached. All 272 fixed historical extraction checkpoints
were replayed, containing 49 memories, without retuning or regenerating them.
Ingestion completion is not an extraction-quality measurement.

### Configuration, verification and accounting

- Reader/judge: `grok-4.3`, reasoning disabled; local `bge-m3` embeddings through
  `kenfold-embed`, `bge-reranker-v2-m3` reranking, production retrieval top 10.
  The scoring identity pinned input bytes, historical extraction projections,
  code/binary, local weights/profiles and inference settings. A hosted model
  alias is not a guarantee of immutable provider-side weights.
- Independent checks matched all saved record checksums and exact dataset,
  question, model, prompt and timestamp keys; recomputed F1 from saved answers;
  checked verdicts and recall validity; and reproduced every report cell.
  Verification made **no reader/judge calls** and left checkpoints and accounting
  unchanged. Saved recall was validated and aggregated, not rerun retrieval.
- The journal recorded 2,807 health/reader/judge calls and reported
  1,791,829 input / 13,959 output tokens. At the verified uncached prices, known
  token usage estimates **$2.27468375**. The unknown final call retains its full
  **$0.05** reservation, giving a journal estimate of **$2.32468375**.
- Pending/reserved counters are zero because the uncertain reservation was
  retained as conservative spend. **They do not resolve the unknown judge call.**
  The journal stop and in-flight checkpoint prohibit blind automatic retries.
- Known token-price estimates across all accounted attempts total
  **$5.95782375**, excluding uncertain usage. These are estimates, not invoices.
  The original **$4.50** allocation envelopes and the new **$5.00** envelope remain
  retained: **$9.50** conservatively allocated within the explicitly approved
  **$10** cumulative cap. No unused allowance was released or reset.
- No new paid retry or commit/push followed completion. The dedicated Ollama was
  stopped but its container/model volume retained. Both evaluation databases
  remain preserved; the original database still has 7,185 memories. The new
  5,931-memory database was privately backed up and its archive listing checked;
  a full restore was not tested.

Score identity:
`5db137baee808d72a5fe4fa2b1c1f1e0e48f8c7f16e82c5acf148887f9a98841`.
Logs, individual answers/verdicts, journals, input manifests and backups remain
private with the task. The earlier outcome below is historical, not a claim that
this retry recovered the earlier run's lost scores.

## Earlier unsuccessful attempt — historical record

The 2026-10-07 run exited unsuccessfully. **No accuracy, F1, or evidence-recall
scores from this run are available or claimed. Phase 5 validation is incomplete.**
The completion counters below describe processing, not measured quality.

| Evaluation | Selected questions | Logged question processing | Recoverable quality scores |
|---|---:|---:|---|
| LoCoMo categories 1–4, raw-turn memories | 1,540 | 1,540 | None |
| LoCoMo categories 1–4, extracted memories | 1,540 | 1,540 | None |
| LongMemEval_S, stratified subset | 60 | 0 | None |

All 272 LoCoMo extraction sessions completed. LongMemEval ingested five of
60 question haystacks, then failed while embedding the sixth (`15745da0_abs`)
through local Ollama. No LongMemEval questions were answered or judged.
The configured embedding client deadline was three minutes; the request ended
with `Client.Timeout exceeded while awaiting headers`.

## Why no quality scores are published

The harness kept completed LoCoMo score aggregates in memory. Its non-budget
error branch called `t.Fatal` before logging or writing the report, so the later
LongMemEval ingestion failure discarded those aggregates. The saved progress,
token accounting, and extraction checkpoints contain neither predictions nor
judge verdicts and cannot reconstruct the scores. The four-question smoke run
is not a substitute for public benchmark validation.

The report error path has since been corrected and regression-tested with local
fixtures: completed dataset results are written with a `PARTIAL` note before
the harness reports failure. This report-only fix does **not** recover the lost
scores or itself add per-question scoring checkpoints. No paid rerun was performed.

## Configuration and recovery caveats

- Reader and judge: `grok-4.3`, reasoning disabled. Embeddings: `bge-m3` through
  the local `kenfold-embed` alias; reranker: `bge-reranker-v2-m3`.
- Extraction used the unchanged production `kenfold-extract` model, prompt and
  settings, not an extractor retuned for LoCoMo.
- The continuation replayed 87 evidenced session projections: 85 explicitly
  empty results and seven stored memories from two sessions. These are recovered
  ingestion projections, not original model transcripts. Three lost positive
  sessions (`conv-42/D2`, `D8`, `D12`) were regenerated, alongside 182 unfinished
  sessions; outputs were not selected according to benchmark scores.

## Accounting

The continuation settled 6,161 health/reader/judge calls, with 2,885,543 input and
24,422 output tokens. Its uncached token-price upper estimate is **$3.667984**,
below its **$3.84** ceiling. The durable journal has no pending calls or reserved
in-flight amount. This is an estimate, not a provider invoice.

Known token-price estimates across accounted runs total **$3.683140**. Earlier
interrupted usage is still retained conservatively: $0.25 for the original
smoke and a $0.15 health-call upper bound inside the prior main-retry envelope.
The unchanged conservative allocation envelopes total **$4.50**, below the
user's **$5** limit. After this failure, no allowance was reset or released and
no new paid retry was allocated.

Completion: `2026-10-07T08:12:09Z`, exit status `1`.

Evidence retained privately with the task:

- Final log SHA-256: `c0cf01f629aaf752076559bad3a2a3247435cc80ce1b58646740b89c4e6d95db`
- Usage journal SHA-256: `645faa1f65f0f81dad888adabdbde294019966dff59a11e7e77b75706f4b4025`

This outcome report is not a successful benchmark result or a comparison with
other systems. A future measurement needs explicit authorization and durable
score preservation; this attempt cannot establish memory or answer quality.

## Free recovery verification — no quality scores

A subsequent **local-only embedding check**, not a QA benchmark, exited `0` at
`2026-10-07T20:13:30Z`. It used the unchanged stratified 60-question LongMemEval_S
selection, production parsing and 2,000-byte clipping, batches of at most 32,
the same local embedding weights, and the unchanged three-minute client deadline.

- All **60 haystacks**, **494 embedding HTTP batches**, and **14,776 vectors**
  completed. Every vector was checked for 1,024 dimensions and finite values.
- The formerly failing input `15745da0_abs` completed all nine batches (278 vectors).
- The longest batch took **54.332 seconds**; the full check took about **5 h 10 m**.
- There were **zero reader/judge or database calls**, no new external API charges,
  and no new paid retry or budget reset.

Separately, optional per-question score checkpoints were implemented and tested
with free local HTTP fixtures and a separate disposable database. Real subprocess
SIGKILL/reopen checks preserved completed answers, verdicts, F1 and evidence recall
without repeating completed-question calls; uncertain in-flight calls stopped
automatic retries. Fixture values are **not** public benchmark scores. Startup
health checks and ingestion remain separate from per-question replay.

These checks do **not** recover the historical scores, measure QA quality, or
complete Phase 5. The accounting above is unchanged. The retained local-check log
SHA-256 is `edcd335a4fe7225ae92a399691b985e0a228ba640444407995cfc31398107925`.
