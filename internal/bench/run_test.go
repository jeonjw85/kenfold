package bench

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/embed"
	"github.com/kenfold/kenfold/internal/rerank"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

// TestBench scores LoCoMo (categories 1-4) and a LongMemEval_S subset.
// It is opt-in and TRUNCATES the memory table of the test database:
//
//	KENFOLD_TEST_DATABASE_URL=postgres://.../kenfold_test \
//	KENFOLD_EVAL_EMBED_URL=http://127.0.0.1:11435/v1 \
//	KENFOLD_BENCH_CHAT_URL=https://api.openai.com/v1 KENFOLD_BENCH_API_KEY=sk-... \
//	  go test -run TestBench -v -timeout 8h ./internal/bench/
//
// KENFOLD_BENCH_EXTRACT=1 also runs the production extractor on LoCoMo
// (KENFOLD_EVAL_CHAT_URL). KENFOLD_BENCH_LIMIT caps questions per dataset.
// KENFOLD_BENCH_BUDGET_USD bounds reader/judge API calls when token prices
// KENFOLD_BENCH_INPUT_USD_PER_M and KENFOLD_BENCH_OUTPUT_USD_PER_M are provided.
// Prices must cover both models and context tiers; reasoning must be "none".
// KENFOLD_BENCH_USAGE_FILE persists that allowance across process restarts.
// KENFOLD_BENCH_EXTRACT_CACHE with KENFOLD_BENCH_EXTRACT_ID checkpoints sessions;
// the identity must pin weights, extractor code/schema and inference settings.
// KENFOLD_BENCH_SCORE_CACHE with KENFOLD_BENCH_SCORE_ID checkpoints each QA phase;
// pin dataset/input state, scoring/retrieval code and all model settings. Reuse
// the same private cache and budget journal on restart; uncertain calls stop.
// Results are written to testdata/RESULTS.md.
func TestBench(t *testing.T) {
	answerOptions, err := answerOptionsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	dbURL := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	embedURL := os.Getenv("KENFOLD_EVAL_EMBED_URL")
	chatURL := os.Getenv("KENFOLD_BENCH_CHAT_URL")
	if dbURL == "" || embedURL == "" || chatURL == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL, KENFOLD_EVAL_EMBED_URL, and KENFOLD_BENCH_CHAT_URL not set")
	}
	var scoreCache *ScoreCache
	if dir, identity := os.Getenv("KENFOLD_BENCH_SCORE_CACHE"), os.Getenv("KENFOLD_BENCH_SCORE_ID"); dir != "" || identity != "" {
		var err error
		scoreCache, err = NewScoreCache(dir, identity)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scoreCache.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	if err := validateAnswerOptions(answerOptions, scoreCache); err != nil {
		t.Fatal(err)
	}
	budget, err := budgetFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("KENFOLD_BENCH_USAGE_FILE"); path != "" {
		if budget == nil {
			t.Fatal("KENFOLD_BENCH_USAGE_FILE requires a configured budget")
		}
		if os.Getenv("KENFOLD_BENCH_USAGE_CREATE") == "1" {
			err = budget.initJournal(path)
		} else {
			err = budget.enableJournal(path)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := budget.closeJournal(); err != nil {
				t.Error(err)
			}
		}()
	}
	if budget != nil {
		defer func() { t.Log(budget.Note()) }()
	}
	ctx := context.Background()
	dir := os.Getenv("KENFOLD_BENCH_DATA")
	if dir == "" {
		var err error
		dir, err = DefaultDir()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	emb, err := embed.New(embed.Config{
		BaseURL: embedURL,
		Model:   envOr("KENFOLD_EVAL_EMBED_MODEL", "kenfold-embed"),
		Name:    envOr("KENFOLD_EVAL_EMBED_NAME", "bge-m3"),
		Dim:     store.EmbeddingDim,
		Timeout: 3 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := benchChat(chatURL, envOr("KENFOLD_BENCH_READER", "gpt-4o-mini"))
	if err != nil {
		t.Fatal(err)
	}
	judgeModel := envOr("KENFOLD_BENCH_JUDGE", "gpt-4o")
	judge := reader
	if judgeModel != reader.Model() {
		judge, err = benchChat(envOr("KENFOLD_BENCH_JUDGE_URL", chatURL), judgeModel)
		if err != nil {
			t.Fatal(err)
		}
	}
	d := Deps{Store: store.New(pool), Pool: pool, Embedder: emb, Reader: reader, Judge: judge, ScoreCache: scoreCache, Budget: budget, Log: t.Logf, AnswerOptions: answerOptions}
	if dir := os.Getenv("KENFOLD_BENCH_EXTRACT_CACHE"); dir != "" {
		d.ExtractCache, err = NewExtractionCache(dir, os.Getenv("KENFOLD_BENCH_EXTRACT_ID"))
		if err != nil {
			t.Fatal(err)
		}
	}
	if u := os.Getenv("KENFOLD_EVAL_RERANK_URL"); u != "" {
		rc, err := rerank.New(rerank.Config{BaseURL: u, Model: envOr("KENFOLD_EVAL_RERANK_MODEL", "bge-reranker-v2-m3"), Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		d.Reranker = rc
	}
	cfg := Config{DataDir: dir, LMEN: envInt("KENFOLD_BENCH_LME_N", DefaultLMEN), Limit: envInt("KENFOLD_BENCH_LIMIT", 0)}
	if os.Getenv("KENFOLD_BENCH_EXTRACT") == "1" {
		exURL := os.Getenv("KENFOLD_EVAL_CHAT_URL")
		if exURL == "" {
			t.Fatal("KENFOLD_BENCH_EXTRACT=1 requires KENFOLD_EVAL_CHAT_URL")
		}
		ex, err := chat.New(chat.Config{
			BaseURL: exURL, Model: envOr("KENFOLD_EVAL_CHAT_MODEL", "kenfold-extract"),
			Timeout: 3 * time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		d.Extractor = ex
		cfg.Extract = true
	}

	rep, runErr := Run(ctx, d, cfg)
	reportBench(t, rep, runErr, budget, cfg.Limit)
}

// Reporting must precede failure for every Run error, not only budget stops.
func reportBench(t *testing.T, rep Report, runErr error, budget *Budget, limit int) {
	t.Helper()
	if budget != nil {
		rep.Notes = append(rep.Notes, budget.Note())
	}
	if limit > 0 {
		rep.Notes = append(rep.Notes, "PARTIAL: question-limited run in dataset order; not the complete LoCoMo benchmark. A limit that truncates the LongMemEval selection does not create a new stratified subset.")
	}
	if runErr != nil {
		if !errors.Is(runErr, ErrBudgetExceeded) && !errors.Is(runErr, ErrBudgetUsageUnavailable) {
			rep.Notes = append(rep.Notes, "PARTIAL: evaluation stopped after a benchmark error. Only completed results are included; unfinished questions and datasets were not evaluated.")
		} else {
			rep.Notes = append(rep.Notes, "PARTIAL: evaluation stopped to protect the API budget. Only completed answer/judge pairs are included; remaining questions and datasets were not evaluated.")
		}
	}
	md := rep.Markdown()
	t.Log("\n" + md)
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("testdata/RESULTS.md", []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Error(runErr)
	}
	for _, s := range rep.Sets {
		for _, row := range s.Rows {
			if row.Label == "all" && row.Errors > 0 && row.N+row.Errors > 0 && float64(row.Errors)/float64(row.N+row.Errors) > 0.05 {
				t.Errorf("%s: %d/%d questions failed", s.Name, row.Errors, row.N+row.Errors)
			}
		}
	}
}

func benchChat(baseURL, model string) (*chat.Client, error) {
	reasoning := os.Getenv("KENFOLD_BENCH_REASONING")
	if reasoning == "" {
		reasoning = chat.ReasoningOmit
	}
	return chat.New(chat.Config{
		BaseURL:   baseURL,
		Model:     model,
		APIKey:    os.Getenv("KENFOLD_BENCH_API_KEY"),
		Reasoning: reasoning,
		Timeout:   3 * time.Minute,
	})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
