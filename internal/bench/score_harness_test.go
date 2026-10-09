package bench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/store"
)

// Invoke the actual opt-in harness in a scratch working directory: fixture
// reports must never overwrite the retained public unsuccessful outcome.
func runBenchFixture(t *testing.T, dir string, env []string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBench$", "-test.v", "-test.timeout=45s")
	cmd.Dir = dir
	cmd.Env = append(scoreChildEnv(), env...)
	return cmd.CombinedOutput()
}

// Missing/unsafe cache options must stop before data loading, DB migration or
// model health checks, rather than silently leaving scoring uncheckpointed.
func TestBenchScoreCheckpointRejectsInvalidConfiguration(t *testing.T) {
	for _, mode := range []string{"missing-id", "missing-dir", "public-dir", "wrong-identity", "locked"} {
		t.Run(mode, func(t *testing.T) {
			work := t.TempDir()
			dir, identity := filepath.Join(work, "scores"), "harness-fixture-v1"
			switch mode {
			case "missing-id":
				identity = ""
			case "missing-dir":
				dir = ""
			case "public-dir":
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			case "wrong-identity", "locked":
				cache, err := NewScoreCache(dir, identity)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { cache.Close() })
				if mode == "wrong-identity" {
					if err := cache.Close(); err != nil {
						t.Fatal(err)
					}
					identity = "different-harness-fixture"
				}
			}
			// If validation is missing, this regular file stops Ensure without
			// downloading anything or touching the intentionally unreachable DB.
			dataPath := filepath.Join(work, "not-a-data-directory")
			if err := os.WriteFile(dataPath, []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runBenchFixture(t, work, []string{
				"KENFOLD_TEST_DATABASE_URL=postgres://fixture@127.0.0.1:1/unused?sslmode=disable",
				"KENFOLD_EVAL_EMBED_URL=http://127.0.0.1:1",
				"KENFOLD_BENCH_CHAT_URL=http://127.0.0.1:1",
				"KENFOLD_BENCH_DATA=" + dataPath,
				"KENFOLD_BENCH_SCORE_CACHE=" + dir,
				"KENFOLD_BENCH_SCORE_ID=" + identity,
			})
			if err == nil || !strings.Contains(string(out), ErrScoreCheckpoint.Error()) {
				t.Fatalf("invalid scoring-cache options did not stop the harness before data/DB/model work: %v\n%s", err, out)
			}
		})
	}
}

// The missing Deps.ScoreCache wiring would score both questions again on the
// second invocation. Fixtures cover only the external HTTP boundary; parsing,
// ingestion, retrieval, scoring, durable files, accounting and reporting are real.
func TestBenchScoreCheckpointReplay(t *testing.T) {
	scoreFixturePool(t) // Enforce the separately named, credential-free test DB.
	var health, reader, judge atomic.Int32
	var changed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("harness fixture received credentials")
			http.Error(w, "unexpected credentials", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/chat/completions":
			var req struct {
				Model     string
				Messages  []chat.Message
				MaxTokens int `json:"max_tokens"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				return
			}
			text := "blue"
			if req.MaxTokens == 8 && len(req.Messages) == 1 && req.Messages[0].Content == "Reply with the single word ok." {
				health.Add(1)
				text = "ok"
			} else if req.Model == "fixture-reader" && req.MaxTokens == 256 {
				reader.Add(1)
				if changed.Load() {
					text = "red"
				}
			} else if req.Model == "fixture-judge" && req.MaxTokens == 16 {
				judge.Add(1)
				text = "yes"
				if changed.Load() {
					text = "no"
				}
			} else {
				t.Error("unexpected harness completion")
			}
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": text}}},
				"usage":   map[string]int{"prompt_tokens": 20, "completion_tokens": 1},
			})
		case "/embeddings":
			var req struct{ Input []string }
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				return
			}
			data := make([]map[string]any, len(req.Input))
			for i := range req.Input {
				vec := make([]float32, store.EmbeddingDim)
				vec[0] = 1
				data[i] = map[string]any{"index": i, "embedding": vec}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		default:
			t.Error("unexpected harness fixture endpoint")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]string{
		"locomo10.json":              `[{"sample_id":"fixture-score","qa":[{"question":"What color?","answer":"blue","evidence":["D1:1"],"category":2}],"conversation":{"session_1_date_time":"1:56 pm on 8 May, 2023","session_1":[{"speaker":"User","dia_id":"D1:1","text":"The chosen color is blue."}]}}]`,
		"longmemeval_s_cleaned.json": `[{"question_id":"fixture-score-lme","question_type":"single-session-user","question":"What color?","question_date":"2023/05/30 (Tue) 23:40","answer":"blue","answer_session_ids":["s1"],"haystack_dates":["2023/05/20 (Sat) 02:21"],"haystack_session_ids":["s1"],"haystack_sessions":[[{"role":"user","content":"The chosen color is blue."},{"role":"assistant","content":"Noted."}]]}]`,
	}
	for name, fixture := range fixtures {
		// Ensure intentionally requires >1000 bytes to accept a cached file.
		if err := os.WriteFile(filepath.Join(dataDir, name), []byte(fixture+strings.Repeat(" ", 1100)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cacheDir := filepath.Join(work, "scores")
	journal := filepath.Join(work, "budget.json")
	env := []string{
		"KENFOLD_TEST_DATABASE_URL=" + os.Getenv("KENFOLD_SCORE_TEST_DATABASE_URL"),
		"KENFOLD_EVAL_EMBED_URL=" + server.URL,
		"KENFOLD_EVAL_EMBED_MODEL=fixture-embedding",
		"KENFOLD_BENCH_CHAT_URL=" + server.URL,
		"KENFOLD_BENCH_READER=fixture-reader",
		"KENFOLD_BENCH_JUDGE=fixture-judge",
		"KENFOLD_BENCH_DATA=" + dataDir,
		"KENFOLD_BENCH_LME_N=1",
		"KENFOLD_BENCH_SCORE_CACHE=" + cacheDir,
		"KENFOLD_BENCH_SCORE_ID=harness-fixture-dataset-memory-code-inference-v1",
		"KENFOLD_BENCH_BUDGET_USD=1",
		"KENFOLD_BENCH_INPUT_USD_PER_M=1",
		"KENFOLD_BENCH_OUTPUT_USD_PER_M=1",
		"KENFOLD_BENCH_REASONING=none",
		"KENFOLD_BENCH_USAGE_FILE=" + journal,
	}
	for attempt := 0; attempt < 2; attempt++ {
		create := "0"
		if attempt == 0 {
			create = "1"
		} else {
			changed.Store(true)
		}
		out, err := runBenchFixture(t, work, append(append([]string(nil), env...), "KENFOLD_BENCH_USAGE_CREATE="+create))
		if err != nil {
			t.Fatalf("fixture harness failed: %v\n%s", err, out)
		}
		if reader.Load() != 2 || judge.Load() != 2 || health.Load() != int32((attempt+1)*2) {
			t.Fatalf("harness repeated completed scoring or skipped existing health checks: reader=%d judge=%d health=%d", reader.Load(), judge.Load(), health.Load())
		}
		paths, err := filepath.Glob(filepath.Join(cacheDir, "question-*.json"))
		if err != nil || len(paths) != 2 {
			t.Fatalf("harness did not publish both per-question checkpoints: %d files, %v", len(paths), err)
		}
		md, err := os.ReadFile(filepath.Join(work, "testdata", "RESULTS.md"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"## LoCoMo categories 1-4, raw turns", "## LongMemEval_S subset (1, stratified by type)",
			"| all | 1 | 1.000 | 1.000 | 1.000 | 1.000 | 0 |", "| all | 1 | 1.000 | 1.000 | 1.000 | 0 |",
		} {
			if !strings.Contains(string(md), want) {
				t.Fatalf("harness report lost the original fixture scores: missing %q\n%s", want, md)
			}
		}
		var rec budgetRecord
		if err := readPrivateJSON(journal, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.State.Calls != 6+attempt*2 || rec.State.PendingCalls != 0 {
			t.Fatalf("harness restart reset or duplicated accounting: %+v", rec.State)
		}
	}
	t.Log("actual TestBench reopened the same score cache and allowance; completed reader/judge calls repeated 0 times; two existing health calls per start remain")
}
