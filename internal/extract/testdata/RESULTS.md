# Extraction eval results

Run with `make eval` (needs `make up-extract`). Model: `kenfold-extract` = `qwen3.5:4b` on Ollama 0.34.4, CPU in Docker, 4 threads, `reasoning_effort: none`, temperature 0. Machine: Apple M5 (4 performance + 6 efficiency cores). Measured 2026-09-27.

The dev cases were used to write and tune the prompt; the holdout cases were written afterwards and not used for tuning. With 16 expected memories in total these are sanity checks, not benchmarks.

| | Result |
|---|---|
| Extraction recall, dev | 11/11 |
| Extraction recall, holdout | 4/5 |
| Forbidden extractions (secrets, injected instructions, one-off tasks) | 0 |
| Classification accuracy | 10/10 |
| Time per session summary | 12–32 s |

## What changed during tuning

The first prompt listed what to extract and what to skip. On the dev set it reached 55% recall with 7 forbidden extractions:

- It extracted instructions from a pasted README ("disable TLS verification", "push to main without review") as preferences.
- It extracted one-off tasks ("the copyright year was updated").
- It typed project conventions as preferences, and reported confidence 1.00 for everything.

The current prompt asks the model to put every item in a category, including two discard categories (`task_or_status`, `from_pasted_content`). It reports a categorical `basis` (stated or inferred) instead of a number. Code then drops the discard categories.

## What the numbers do not show

- **Pasted-content defense is mechanical.** In both injection cases the model still proposed the injected instructions, labeled as ordinary categories. They were blocked because their evidence lies inside quoted text (`evidence is inside quoted text`). Instructions pasted without quotes, or paraphrased by the assistant in its final response, would not be caught by that check. This is why extracted memories are `proposed` by default and served only after review.
- **Evidence grounding catches invented memories.** The model twice produced "Answer in Korean" (copied from an example in the prompt) for sessions that never mention it. It was rejected because its evidence does not occur in the session.
- **Holdout miss.** "We're moving from Jest to Vitest" was extracted as a code fact instead of a project rule. Stored, it would still be found by `recall`, but it would not be injected into `get_context` as project knowledge.
