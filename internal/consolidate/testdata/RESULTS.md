# Consolidation eval

`make eval` (with `make up-extract` running) judges the 12 labeled pairs in `eval.json` and writes one digest, with `kenfold-extract` (qwen3.5:4b, 4 threads) on an Apple M5 CPU. Each pair is two memories of the same project, A written three days before B. The set is small and synthetic: it checks that the prompts work as intended, not how well consolidation does on real memory.

"False retire" counts distinct pairs judged duplicate or conflict. Applying such a proposal would retire a memory that is still needed, so it is the error that matters most.

| Run | Change | Correct | False retire | Wrong keep | Missed | Time per pair |
|---|---|---|---|---|---|---|
| 1 | first prompt, one reading | 11/12 | 0 | 0 | 1 (same-older-detailed: judged distinct) | 7.1 s |
| 2 | "either one may be the more detailed one", an example that keeps A | 11/12 | 1 (distinct-details: judged same) | 0 | 0 | 7.4 s |
| 3 | a second reading with A and B swapped; proposed only if both agree | **12/12** | **0** | 0 | 0 | 13.3 s |

Run 3 is the shipped version. The second reading rejected the false "same" of run 2, because the model's answer changed with the order. Distinct pairs cost one call (about 7.5 s) unless the first reading proposes something.

The digest of the six sessions (English and Korean) passed validation: every file name, identifier, and number in it appears in the sessions. It took 7–12 s and kept the order, the decisions, and the open question.
