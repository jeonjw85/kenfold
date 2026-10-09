package bench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/embed"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

// Only the separately provisioned disposable recovery DB is allowed here.
// No .env loading, database passwords in URLs, or live model endpoints.
func scoreFixturePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("KENFOLD_SCORE_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("KENFOLD_SCORE_TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(dbURL)
	if err != nil || u.User == nil || !strings.HasPrefix(u.Path, "/kenfold_score_recovery_") || os.Getenv("PGPASSFILE") == "" {
		t.Fatal("a credential-free isolated score recovery DB and PGPASSFILE are required")
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		t.Fatal("database URL must not contain credentials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := migrations.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type scoreHTTPFixture struct {
	mu                       sync.Mutex
	server                   *httptest.Server
	reader, judge, embedding atomic.Int32
	beforeChat               func(string)
	inspectChat              func(string, []chat.Message)
	beforeEmbed              func([]string)
	answer, verdict          string
	failModel                string
}

func newScoreHTTPFixture(t *testing.T) *scoreHTTPFixture {
	t.Helper()
	f := &scoreHTTPFixture{answer: "blue", verdict: "yes"}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("fixture received an authorization header")
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
			f.mu.Lock()
			answer, verdict, failModel, beforeChat, inspectChat := f.answer, f.verdict, f.failModel, f.beforeChat, f.inspectChat
			f.mu.Unlock()
			text := answer
			switch req.Model {
			case "fixture-reader":
				f.reader.Add(1)
				if req.MaxTokens != 256 || len(req.Messages) != 2 || (inspectChat == nil && req.Messages[0].Content != readerSystem) {
					t.Error("unexpected reader request")
				}
			case "fixture-judge":
				f.judge.Add(1)
				text = verdict
				if req.MaxTokens != 16 || len(req.Messages) != 1 || !strings.Contains(req.Messages[0].Content, "Model Response: "+answer) {
					t.Error("judge did not receive the saved reader answer")
				}
			default:
				t.Error("unexpected model")
			}
			if beforeChat != nil {
				beforeChat(req.Model)
			}
			if inspectChat != nil {
				inspectChat(req.Model, req.Messages)
			}
			finish := "stop"
			if failModel == req.Model {
				finish = "length"
			}
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"finish_reason": finish, "message": map[string]any{"content": text}}},
				"usage":   map[string]int{"prompt_tokens": 20, "completion_tokens": 1},
			})
		case "/embeddings":
			f.embedding.Add(1)
			var req struct{ Input []string }
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				return
			}
			f.mu.Lock()
			beforeEmbed := f.beforeEmbed
			f.mu.Unlock()
			if beforeEmbed != nil {
				beforeEmbed(req.Input)
			}
			data := make([]map[string]any, len(req.Input))
			for i := range req.Input {
				vec := make([]float32, store.EmbeddingDim)
				vec[0] = 1
				data[i] = map[string]any{"index": i, "embedding": vec}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		default:
			t.Error("unexpected fixture endpoint")
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func scoreFixtureDeps(t *testing.T, pool *pgxpool.Pool, f *scoreHTTPFixture) Deps {
	t.Helper()
	reader, err := chat.New(chat.Config{BaseURL: f.server.URL, Model: "fixture-reader", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	judge, err := chat.New(chat.Config{BaseURL: f.server.URL, Model: "fixture-judge", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	emb, err := embed.New(embed.Config{BaseURL: f.server.URL, Model: "fixture-embedding", Dim: store.EmbeddingDim})
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Store: store.New(pool), Pool: pool, Embedder: emb, Reader: reader, Judge: judge}
}

func scoreFixtureQuestions(t *testing.T, d Deps) []scoredQ {
	t.Helper()
	scope := "project:score-recovery-" + sha256Hex([]byte(t.Name()))[:16]
	t.Cleanup(func() {
		if _, err := d.Pool.Exec(context.Background(), `DELETE FROM memory WHERE scope = $1`, scope); err != nil {
			t.Error(err)
		}
	})
	when := time.Date(2023, 5, 8, 13, 56, 0, 0, time.UTC)
	if err := ingest(context.Background(), d, scope, []draft{{Content: "The chosen color is blue.", DiaID: "D1:1", When: when}}); err != nil {
		t.Fatal(err)
	}
	return []scoredQ{
		{Scope: scope, Question: Question{ID: "first", Type: "2", Text: "What color?", Answer: "blue", Evidence: []string{"D1:1"}, When: when}},
		{Scope: scope, Question: Question{ID: "second", Type: "1", Text: "Which color?", Answer: "blue", Evidence: []string{"D1:1"}, When: when}},
	}
}

func TestScoreRecoveryLaterSearchRetainsRows(t *testing.T) {
	pool := scoreFixturePool(t)
	for _, mode := range []string{"nil-cache", "cached-search-error", "cached-local-error"} {
		t.Run(mode, func(t *testing.T) {
			f := newScoreHTTPFixture(t)
			d := scoreFixtureDeps(t, pool, f)
			qs := scoreFixtureQuestions(t, d)
			dir := filepath.Join(t.TempDir(), "scores")
			if mode != "nil-cache" {
				c, err := NewScoreCache(dir, "fixture-v1")
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				d.ScoreCache = c
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.mu.Lock()
			f.beforeEmbed = func(input []string) {
				if input[0] == "Which color?" {
					if mode == "cached-local-error" {
						if err := os.Chmod(dir, 0o755); err != nil {
							t.Error(err)
						}
					} else {
						cancel()
					}
				}
			}
			f.mu.Unlock()
			rows, err := scoreQuestions(ctx, d, qs, true, t.Logf)
			if err == nil || (mode != "cached-local-error" && !strings.Contains(err.Error(), "search second")) || (mode == "cached-local-error" && !strings.Contains(err.Error(), "checkpoint")) {
				t.Fatalf("want later stage failure, got %v", err)
			}
			want := []Row{
				{Label: "all", N: 1, Judge: 1, F1: 1, HasF1: true, Recall5: 1, Recall10: 1, RecallN: 1},
				{Label: LoCoMoCategory("2"), N: 1, Judge: 1, F1: 1, HasF1: true, Recall5: 1, Recall10: 1, RecallN: 1},
			}
			if !reflect.DeepEqual(rows, want) {
				t.Fatalf("later failure discarded completed rows: got %+v, want %+v", rows, want)
			}
			if f.reader.Load() != 1 || f.judge.Load() != 1 {
				t.Fatalf("unexpected calls: reader=%d judge=%d", f.reader.Load(), f.judge.Load())
			}
		})
	}
}

func TestScoreRecoveryCompletedReplay(t *testing.T) {
	pool := scoreFixturePool(t)
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, pool, f)
	qs := scoreFixtureQuestions(t, d)
	uncached, err := scoreQuestions(context.Background(), d, qs, true, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "scores")
	c, err := NewScoreCache(cacheDir, "fixture-dataset-memory-code-inference-v1")
	if err != nil {
		t.Fatal(err)
	}
	d.ScoreCache = c
	cached, err := scoreQuestions(context.Background(), d, qs, true, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cached, uncached) {
		t.Fatalf("cached scoring changed rows: got %+v want %+v", cached, uncached)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = NewScoreCache(cacheDir, "fixture-dataset-memory-code-inference-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d.ScoreCache = c
	// A replay must not even try retrieval. Use an actually closed pool.
	dead, err := pgxpool.NewWithConfig(context.Background(), pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	dead.Close()
	d.Store = store.New(dead)
	reader, judge, embedding := f.reader.Load(), f.judge.Load(), f.embedding.Load()
	replayed, err := scoreQuestions(context.Background(), d, qs, true, t.Logf)
	if err != nil {
		t.Fatalf("durable completed results were not replayed: %v", err)
	}
	if !reflect.DeepEqual(replayed, uncached) {
		t.Fatalf("replay duplicated or lost accumulators: got %+v want %+v", replayed, uncached)
	}
	if f.reader.Load() != reader || f.judge.Load() != judge || f.embedding.Load() != embedding {
		t.Fatal("completed replay made retrieval/reader/judge HTTP calls")
	}
}

func TestScoreRecoveryReaderDoneBudgetBoundary(t *testing.T) {
	pool := scoreFixturePool(t)
	f := newScoreHTTPFixture(t)
	f.mu.Lock()
	f.answer = ""  // A successful empty answer must still be durably available.
	f.verdict = "" // Likewise, an empty judge response is a real "not yes" result.
	f.mu.Unlock()
	d := scoreFixtureDeps(t, pool, f)
	qs := scoreFixtureQuestions(t, d)[:1]
	dir := filepath.Join(t.TempDir(), "scores")
	c, err := NewScoreCache(dir, "fixture-v1")
	if err != nil {
		t.Fatal(err)
	}
	d.ScoreCache = c
	d.Budget, err = NewBudget(0.05, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := scoreQuestions(context.Background(), d, qs, true, t.Logf)
	if err != ErrBudgetExceeded || len(rows) != 0 {
		t.Fatalf("want blocked judge, got %+v %v", rows, err)
	}
	if f.reader.Load() != 1 || f.judge.Load() != 0 {
		t.Fatal("reservation cutoff occurred after network")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = NewScoreCache(dir, "fixture-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d.ScoreCache, d.Budget = c, nil // Free fixtures only, no persisted allowance is reset.
	reader, embedding := f.reader.Load(), f.embedding.Load()
	rows, err = scoreQuestions(context.Background(), d, qs, true, t.Logf)
	if err != nil {
		t.Fatalf("blocked reservation became an uncertain paid call: %v", err)
	}
	want := []Row{
		{Label: "all", N: 1, HasF1: true, Recall5: 1, Recall10: 1, RecallN: 1},
		{Label: LoCoMoCategory("2"), N: 1, HasF1: true, Recall5: 1, Recall10: 1, RecallN: 1},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("reader replay lost metrics: %+v", rows)
	}
	if f.reader.Load() != reader || f.judge.Load() != 1 || f.embedding.Load() != embedding {
		t.Fatalf("reader-only replay repeated reader/retrieval: reader=%d judge=%d embedding=%d", f.reader.Load(), f.judge.Load(), f.embedding.Load())
	}
	rows, err = scoreQuestions(context.Background(), d, qs, true, t.Logf)
	if err != nil || !reflect.DeepEqual(rows, want) || f.reader.Load() != reader || f.judge.Load() != 1 || f.embedding.Load() != embedding {
		t.Fatal("empty scored response became a missing checkpoint")
	}
}

func TestScoreRecoveryIncompatibleQuestionBlocksNetwork(t *testing.T) {
	pool := scoreFixturePool(t)
	for _, change := range []string{"text", "gold", "evidence", "date", "type", "abstain", "with-f1", "reader-model", "judge-model", "endpoint"} {
		t.Run(change, func(t *testing.T) {
			f := newScoreHTTPFixture(t)
			d := scoreFixtureDeps(t, pool, f)
			qs := scoreFixtureQuestions(t, d)[:1]
			c, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "fixture-v1")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			d.ScoreCache = c
			if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err != nil {
				t.Fatal(err)
			}
			withF1 := true
			switch change {
			case "text":
				qs[0].Text += " "
			case "gold":
				qs[0].Answer += " "
			case "evidence":
				qs[0].Evidence = []string{"D2:1"}
			case "date":
				qs[0].When = qs[0].When.Add(time.Second)
			case "type":
				qs[0].Type = "1"
			case "abstain":
				qs[0].Abstain = true
			case "with-f1":
				withF1 = false
			case "reader-model":
				d.Reader, err = chat.New(chat.Config{BaseURL: f.server.URL, Model: "different-reader"})
			case "judge-model":
				d.Judge, err = chat.New(chat.Config{BaseURL: f.server.URL, Model: "different-judge"})
			case "endpoint":
				d.Reader, err = chat.New(chat.Config{BaseURL: f.server.URL + "/changed", Model: "fixture-reader"})
			}
			if err != nil {
				t.Fatal(err)
			}
			reader, judge, embedding := f.reader.Load(), f.judge.Load(), f.embedding.Load()
			if _, err := scoreQuestions(context.Background(), d, qs, withF1, t.Logf); err == nil || !strings.Contains(err.Error(), "checkpoint") {
				t.Fatalf("incompatible question/model was silently replaced: %v", err)
			}
			if f.reader.Load() != reader || f.judge.Load() != judge || f.embedding.Load() != embedding {
				t.Fatal("incompatible key reached network")
			}
		})
	}
}

func TestScoreRecoveryRawExtractedLMEScopesSeparate(t *testing.T) {
	pool := scoreFixturePool(t)
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, pool, f)
	qs := scoreFixtureQuestions(t, d)[:1]
	c, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "fixture-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d.ScoreCache = c
	for _, scope := range []string{"project:bench-locomo-score", "project:bench-locomo-x-score", "project:bench-lme-score"} {
		qs[0].Scope = scope
		if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err != nil {
			t.Fatal(err)
		}
	}
	if f.reader.Load() != 3 || f.judge.Load() != 3 {
		t.Fatal("scope reused a different arm's scored answer")
	}
}

func TestScoreRecoveryUnknownReaderAndJudgeFailClosed(t *testing.T) {
	pool := scoreFixturePool(t)
	for _, model := range []string{"fixture-reader", "fixture-judge"} {
		t.Run(model, func(t *testing.T) {
			f := newScoreHTTPFixture(t)
			f.mu.Lock()
			f.failModel = model
			f.mu.Unlock()
			d := scoreFixtureDeps(t, pool, f)
			qs := scoreFixtureQuestions(t, d)
			dir := filepath.Join(t.TempDir(), "scores")
			c, err := NewScoreCache(dir, "fixture-v1")
			if err != nil {
				t.Fatal(err)
			}
			d.ScoreCache = c
			rows, err := scoreQuestions(context.Background(), d, qs, true, t.Logf)
			if err == nil || len(rows) != 0 {
				t.Fatalf("failed cached call continued/counts errors: %+v %v", rows, err)
			}
			if f.reader.Load() != 1 || (model == "fixture-reader" && f.judge.Load() != 0) || (model == "fixture-judge" && f.judge.Load() != 1) {
				t.Fatal("failed cached call retried or proceeded")
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			c, err = NewScoreCache(dir, "fixture-v1")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			d.ScoreCache = c
			reader, judge, embedding := f.reader.Load(), f.judge.Load(), f.embedding.Load()
			rows, err = scoreQuestions(context.Background(), d, qs, true, t.Logf)
			if err == nil || !strings.Contains(err.Error(), "in-flight") || len(rows) != 0 {
				t.Fatalf("uncertain phase was not explicit: %+v %v", rows, err)
			}
			if f.reader.Load() != reader || f.judge.Load() != judge || f.embedding.Load() != embedding {
				t.Fatal("restart blindly retried an uncertain call")
			}
		})
	}
}

func TestScoreRecoveryCorruptRecordBlocksNetwork(t *testing.T) {
	pool := scoreFixturePool(t)
	for _, corruption := range []string{"json", "checksum", "key", "unknown", "trailing", "public", "symlink", "fifo", "phase", "missing-reader", "missing-judge", "decision", "f1", "recall"} {
		t.Run(corruption, func(t *testing.T) {
			f := newScoreHTTPFixture(t)
			d := scoreFixtureDeps(t, pool, f)
			qs := scoreFixtureQuestions(t, d)[:1]
			dir := filepath.Join(t.TempDir(), "scores")
			c, err := NewScoreCache(dir, "fixture-v1")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			d.ScoreCache = c
			if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err != nil {
				t.Fatal(err)
			}
			path := scoreQuestionRecordPath(t, dir)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch corruption {
			case "json":
				data = []byte("{")
			case "checksum":
				data = []byte(strings.Replace(string(data), `"answer":"blue"`, `"answer":"red"`, 1))
			case "key":
				data = []byte(strings.Replace(string(data), `"id":"first"`, `"id":"other"`, 1))
			case "unknown":
				data = append([]byte(`{"unrecognized":true,`), data[1:]...)
			case "trailing":
				data = append(data, []byte("{}")...)
			case "public":
				err = os.Chmod(path, 0o644)
			case "symlink":
				if err = os.Rename(path, path+"-target"); err == nil {
					err = os.Symlink(path+"-target", path)
				}
			case "fifo":
				if err = os.Remove(path); err == nil {
					err = syscall.Mkfifo(path, 0o600)
				}
			case "phase", "missing-reader", "missing-judge", "decision", "f1", "recall":
				var rec scoreRecord
				if err := json.Unmarshal(data, &rec); err != nil {
					t.Fatal(err)
				}
				switch corruption {
				case "phase":
					rec.State.Phase = "extraction-success"
				case "missing-reader":
					rec.State.Reader = nil
				case "missing-judge":
					rec.State.Judge = nil
				case "decision":
					rec.State.Judge.Yes = false
				case "f1":
					rec.State.Reader.Metrics.F1 = 0
				case "recall":
					rec.State.Reader.Metrics.Recall10 = 2
				}
				state, err := json.Marshal(rec.State)
				if err != nil {
					t.Fatal(err)
				}
				rec.StateSHA256 = sha256Hex(state)
				data, err = json.Marshal(rec)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if corruption != "public" && corruption != "symlink" && corruption != "fifo" {
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			reader, judge, embedding := f.reader.Load(), f.judge.Load(), f.embedding.Load()
			if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err == nil || !strings.Contains(err.Error(), "checkpoint") {
				t.Fatalf("corruption was silently ignored: %v", err)
			}
			if f.reader.Load() != reader || f.judge.Load() != judge || f.embedding.Load() != embedding {
				t.Fatal("corrupt record triggered recomputation")
			}
		})
	}
}

func TestScoreRecoveryReaderReservationBlockedBeforeNetwork(t *testing.T) {
	pool := scoreFixturePool(t)
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, pool, f)
	qs := scoreFixtureQuestions(t, d)[:1]
	dir := filepath.Join(t.TempDir(), "scores")
	c, err := NewScoreCache(dir, "fixture-v1")
	if err != nil {
		t.Fatal(err)
	}
	d.ScoreCache = c
	d.Budget, err = NewBudget(0.001, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err != ErrBudgetExceeded || len(rows) != 0 {
		t.Fatalf("wrong reservation cutoff: %+v %v", rows, err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "question-*.json"))
	if err != nil || len(paths) != 0 || f.reader.Load() != 0 || f.judge.Load() != 0 {
		t.Fatal("blocked reader reservation became in-flight or reached network")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = NewScoreCache(dir, "fixture-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d.ScoreCache = c
	if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err != ErrBudgetExceeded {
		t.Fatalf("restart falsely declared uncertain paid call: %v", err)
	}
	if f.reader.Load() != 0 || f.judge.Load() != 0 {
		t.Fatal("restart bypassed reservation cutoff")
	}
}

func TestScoreRecoveryClosedJournalStopsBeforeNetwork(t *testing.T) {
	pool := scoreFixturePool(t)
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, pool, f)
	qs := scoreFixtureQuestions(t, d)[:1]
	dir := t.TempDir()
	c, err := NewScoreCache(filepath.Join(dir, "scores"), "fixture-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d.ScoreCache = c
	d.Budget, err = NewBudget(1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Budget.initJournal(filepath.Join(dir, "budget.json")); err != nil {
		t.Fatal(err)
	}
	if err := d.Budget.closeJournal(); err != nil {
		t.Fatal(err)
	}
	if rows, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); !errors.Is(err, ErrBudgetUsageUnavailable) || len(rows) != 0 || strings.Contains(err.Error(), "in-flight") {
		t.Fatalf("journal failure was ignored or made uncertain: %+v %v", rows, err)
	}
	if f.reader.Load() != 0 || f.judge.Load() != 0 {
		t.Fatal("closed journal allowed model calls")
	}
}

func TestScoreRecoveryPersistenceFailureStopsModelCalls(t *testing.T) {
	pool := scoreFixturePool(t)
	for _, phase := range []string{"before-reader", "reader-returned", "publish-reader-done"} {
		t.Run(phase, func(t *testing.T) {
			if phase != "reader-returned" && os.Geteuid() == 0 {
				t.Skip("requires filesystem permission enforcement")
			}
			f := newScoreHTTPFixture(t)
			d := scoreFixtureDeps(t, pool, f)
			qs := scoreFixtureQuestions(t, d)
			dir := filepath.Join(t.TempDir(), "scores")
			c, err := NewScoreCache(dir, "fixture-v1")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			d.ScoreCache = c
			t.Cleanup(func() { os.Chmod(dir, 0o700) })
			f.mu.Lock()
			if phase == "before-reader" {
				f.beforeEmbed = func([]string) {
					// Still private/readable, but atomic publication cannot create
					// its temporary file. Exercise writeDurableJSON's real error.
					if err := os.Chmod(dir, 0o500); err != nil {
						t.Error(err)
					}
				}
			} else {
				f.beforeChat = func(model string) {
					if model == "fixture-reader" {
						if phase == "publish-reader-done" {
							if err := os.Chmod(dir, 0o500); err != nil {
								t.Error(err)
							}
						} else {
							if err := os.Rename(dir, dir+"-moved"); err != nil {
								t.Error(err)
							}
							if err := os.Mkdir(dir, 0o700); err != nil {
								t.Error(err)
							}
						}
					}
				}
			}
			f.mu.Unlock()
			rows, err := scoreQuestions(context.Background(), d, qs, true, t.Logf)
			if err == nil || !strings.Contains(err.Error(), "checkpoint") || len(rows) != 0 {
				t.Fatalf("persistence failure became ordinary model error: %+v %v", rows, err)
			}
			wantReader := int32(0)
			if phase != "before-reader" {
				wantReader = 1
			}
			if f.reader.Load() != wantReader || f.judge.Load() != 0 {
				t.Fatal("checkpoint failure allowed further model calls")
			}
			if phase != "reader-returned" {
				if err := os.Chmod(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err == nil {
					t.Fatal("publication failure did not latch fail-closed")
				}
				if f.reader.Load() != wantReader || f.judge.Load() != 0 {
					t.Fatal("ignored cache failure allowed more calls after permission repair")
				}
			}
		})
	}
}
