package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/store"
)

func TestBudgetJudgeStopWritesPartialFailedReportIntegration(t *testing.T) {
	if os.Getenv("KENFOLD_TEST_DATABASE_URL") == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	dir := t.TempDir()
	locomo := `[{"sample_id":"budget","qa":[
		{"question":"First?","answer":"yes","category":1},
		{"question":"Second?","answer":"yes","category":1}],
		"conversation":{"session_1_date_time":"1:56 pm on 8 May, 2023",
		"session_1":[{"speaker":"User","dia_id":"D1:1","text":"yes"}]}}]`
	for name, body := range map[string]string{locomoFile: locomo, lmeFile: "[]"} {
		// Ensure sees these as cached fixtures and performs no downloads.
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+strings.Repeat("\n", 1200)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			calls.Add(1)
			fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"yes"}}],"usage":{"prompt_tokens":20000,"completion_tokens":1}}`)
		case "/embeddings":
			var req struct{ Input []string }
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
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
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cacheDir := filepath.Join(dir, "extraction-cache")
	cache, err := NewExtractionCache(cacheDir, "fixture-pinned")
	if err != nil {
		t.Fatal(err)
	}
	key := cache.Key("budget", "D1", "fixture-extract", "[1:56 pm on 8 May, 2023, session 1, D1:1] User: yes\n")
	if err := cache.Save(key, []ExtractionProjection{{Content: "The user said yes in the session.", Type: "semantic", Confidence: 0.9}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBench$", "-test.v")
	cmd.Dir = dir // The actual harness writes its report here, never over real results.
	cmd.Env = append(os.Environ(),
		"KENFOLD_BENCH_CHAT_URL="+srv.URL, "KENFOLD_BENCH_API_KEY=",
		"KENFOLD_BENCH_READER=test", "KENFOLD_BENCH_JUDGE=test",
		"KENFOLD_BENCH_DATA="+dir, "KENFOLD_BENCH_LIMIT=0", "KENFOLD_BENCH_EXTRACT=1",
		"KENFOLD_EVAL_CHAT_URL="+srv.URL, "KENFOLD_EVAL_CHAT_MODEL=fixture-extract",
		"KENFOLD_BENCH_EXTRACT_CACHE="+cacheDir, "KENFOLD_BENCH_EXTRACT_ID=fixture-pinned",
		"KENFOLD_EVAL_EMBED_URL="+srv.URL, "KENFOLD_EVAL_RERANK_URL=",
		"KENFOLD_BENCH_REASONING=none", "KENFOLD_BENCH_BUDGET_USD=0.14",
		"KENFOLD_BENCH_USAGE_FILE="+filepath.Join(dir, "usage.json"),
		"KENFOLD_BENCH_USAGE_CREATE=1",
		"KENFOLD_BENCH_INPUT_USD_PER_M=1.25", "KENFOLD_BENCH_OUTPUT_USD_PER_M=2.5")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "FAIL") {
		t.Fatalf("unfinished harness reported success: %v\n%s", err, out)
	}
	md, err := os.ReadFile(filepath.Join(dir, "testdata", "RESULTS.md"))
	if err != nil {
		t.Fatalf("partial report not written: %v\n%s", err, out)
	}
	if !strings.Contains(string(md), "PARTIAL") || !strings.Contains(string(md), "| all | 1 |") || !strings.Contains(string(md), "4 calls") {
		t.Fatalf("partial scores/costs not preserved: %s", md)
	}
	// Health check + first reader/judge + second reader; second judge is blocked.
	if calls.Load() != 4 {
		t.Fatalf("judge cutoff did not stop billable calls: %d", calls.Load())
	}
	pool, err := pgxpool.New(ctx, os.Getenv("KENFOLD_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var content, session, typ, status string
	var confidence float64
	var when time.Time
	if err := pool.QueryRow(ctx, `SELECT content, confidence, created_at, attrs->>'bench_session', type, status FROM memory WHERE scope = 'project:bench-locomo-x-budget'`).Scan(&content, &confidence, &when, &session, &typ, &status); err != nil {
		t.Fatalf("cached projection did not reach production ingestion: %v", err)
	}
	if content != "The user said yes in the session." || math.Abs(confidence-0.9) > 1e-6 || session != "D1" || typ != "semantic" || status != "active" || !when.Equal(time.Date(2023, time.May, 8, 13, 56, 0, 0, time.UTC)) {
		t.Fatalf("cached ingestion changed projection/scope/date: content=%q confidence=%v session=%q type=%q status=%q when=%v", content, confidence, session, typ, status, when)
	}
	if _, err := os.Stat(filepath.Join(dir, "usage.json")); err != nil {
		t.Fatalf("actual harness did not persist API accounting: %v", err)
	}
	// Running the real harness again with the same journal must not reset its
	// allowance. Its remaining balance cannot reserve even the health request.
	again := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBench$", "-test.v")
	again.Dir = cmd.Dir
	again.Env = append(cmd.Env, "KENFOLD_BENCH_USAGE_CREATE=0")
	if out, err := again.CombinedOutput(); err == nil {
		t.Fatalf("exhausted resumed harness reported success: %s", out)
	}
	if calls.Load() != 4 {
		t.Fatalf("harness restart reset its paid allowance: %d calls", calls.Load())
	}
}
