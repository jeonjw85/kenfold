package bench

import (
	"context"
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
// Results are written to testdata/RESULTS.md.
func TestBench(t *testing.T) {
	dbURL := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	embedURL := os.Getenv("KENFOLD_EVAL_EMBED_URL")
	chatURL := os.Getenv("KENFOLD_BENCH_CHAT_URL")
	if dbURL == "" || embedURL == "" || chatURL == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL, KENFOLD_EVAL_EMBED_URL, and KENFOLD_BENCH_CHAT_URL not set")
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
	d := Deps{Store: store.New(pool), Pool: pool, Embedder: emb, Reader: reader, Judge: judge, Log: t.Logf}
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

	rep, err := Run(ctx, d, cfg)
	if err != nil {
		t.Fatal(err)
	}
	md := rep.Markdown()
	t.Log("\n" + md)
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("testdata/RESULTS.md", []byte(md), 0o644); err != nil {
		t.Fatal(err)
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
