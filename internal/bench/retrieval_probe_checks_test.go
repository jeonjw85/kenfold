package bench

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/retrieve"
	"github.com/kenfold/kenfold/internal/store"
)

func TestRetrievalProbeNoReaderJudgeOrWrites(t *testing.T) {
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, scoreFixturePool(t), f)
	qs := scoreFixtureQuestions(t, d)[:1]
	ctx := context.Background()
	pool, err := newProbePool(ctx, os.Getenv("KENFOLD_SCORE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var readOnly string
	if err := pool.QueryRow(ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil || readOnly != "on" {
		t.Fatalf("probe DB is writable: %s %v", readOnly, err)
	}
	var before, after string
	query := `SELECT md5(string_agg(row_to_json(m)::text, '' ORDER BY id)) FROM memory m`
	if err := pool.QueryRow(ctx, query).Scan(&before); err != nil {
		t.Fatal(err)
	}
	ret := &retrieve.Retriever{Store: store.New(pool), Embedder: d.Embedder, Options: retrieve.Options{RerankTimeout: 30 * time.Second}}
	report, err := measureRetrieval(ctx, ret, qs)
	if err != nil || len(report.Questions) != 1 || f.reader.Load() != 0 || f.judge.Load() != 0 {
		t.Fatalf("probe incomplete or made completions: %+v %v", report, err)
	}
	if err := pool.QueryRow(ctx, query).Scan(&after); err != nil || before != after {
		t.Fatal("probe altered DB state")
	}
	if _, err := pool.Exec(ctx, `UPDATE memory SET content=content WHERE false`); err == nil {
		t.Fatal("probe connection can write")
	}
}

func TestRetrievalProbeRejectsExternalModelURL(t *testing.T) {
	for _, u := range []string{"https://example.invalid/v1", "http://127.0.0.1@example.invalid/v1", "http://localhost:80/v1", "http://127.0.0.1/v1?key=secret", "http://127.0.0.1/v1#secret"} {
		if err := probeModelURL(u); err == nil {
			t.Errorf("unsafe probe endpoint accepted: %s", u)
		}
	}
	for _, u := range []string{"http://127.0.0.1:80/v1", "http://[::1]:80/v1"} {
		if err := probeModelURL(u); err != nil {
			t.Error(err)
		}
	}
}

type failedProbeEmbedding struct {
	failedBenchEmbedding
	endpoint string
}

func (e failedProbeEmbedding) Endpoint() string { return e.endpoint }

func TestRetrievalProbeRecordsDegradation(t *testing.T) {
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, scoreFixturePool(t), f)
	qs := scoreFixtureQuestions(t, d)[:1]
	ret := &retrieve.Retriever{Store: d.Store, Embedder: failedProbeEmbedding{endpoint: f.server.URL + "/embeddings"}}
	report, err := measureRetrieval(context.Background(), ret, qs)
	if err != nil || len(report.Questions) != 1 || !report.Questions[0].Degraded || len(report.Questions[0].Stages) == 0 {
		t.Fatalf("probe presented failed embedding as normal retrieval: %+v %v", report, err)
	}
}

func TestRetrievalProbeSameQueryAndSourceAcrossArms(t *testing.T) {
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, scoreFixturePool(t), f)
	qs := scoreFixtureQuestions(t, d)[:1]
	cache := &probeEmbeddingCache{inner: d.Embedder, values: map[string][][]float32{}}
	var first retrievalMeasurement
	for i, opts := range probeVariants() {
		ret := &retrieve.Retriever{Store: d.Store, Embedder: cache, Options: opts.Options}
		report, err := measureRetrieval(context.Background(), ret, qs)
		if err != nil {
			t.Fatal(err)
		}
		m := report.Questions[0]
		if i == 0 {
			first = m
		} else if m.ID != first.ID || m.When != first.When || m.QuerySHA256 != first.QuerySHA256 || !reflect.DeepEqual(m.Hits, first.Hits) {
			t.Fatalf("arms changed query/source instead of one option: %s", opts.Name)
		}
	}
	if f.embedding.Load() != 2 || cache.Hits != 2 {
		t.Fatalf("query embedding was not reused (includes ingestion): %d, %d", f.embedding.Load(), cache.Hits)
	}
	variants := probeVariants()
	if len(variants) != 3 || variants[0].Options.Pool != 30 || variants[0].Options.RerankTop != 15 || variants[1].Options.RerankTop != 30 || !variants[2].Options.NoRecency {
		t.Fatal("ablation changed baseline/unapproved options")
	}
	if fmt.Sprint(qs[0].Evidence) != "[D1:1]" {
		t.Fatal("probe mutated labels")
	}
}

func TestRetrievalProbeSelectionDependsOnlyOnIDsNotScoresOrOrder(t *testing.T) {
	s := LoCoMoSample{ID: "unit", Present: time.Date(2023, 5, 8, 0, 0, 0, 0, time.UTC)}
	for _, typ := range []string{"1", "2", "3", "4"} {
		for i := 0; i < 40; i++ {
			s.Questions = append(s.Questions, Question{ID: fmt.Sprintf("q-%s-%d", typ, i), Type: typ, Text: "same", Answer: "old", Evidence: []string{"old"}})
		}
	}
	first, err := probeQuestions([]LoCoMoSample{s}, false)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(s.Questions)
	for i := range s.Questions {
		s.Questions[i].Answer = "new"
		s.Questions[i].Evidence = []string{"new"}
	}
	second, err := probeQuestions([]LoCoMoSample{s}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 128 || len(second) != 128 {
		t.Fatal("not 32 per category")
	}
	for i, q := range first {
		if q.ID != second[i].ID {
			t.Fatal("selection changed with order/labels")
		}
	}
}

func TestRetrievalProbePendingIDsSkipSavedQuestions(t *testing.T) {
	qs := []scoredQ{{Question: Question{ID: "saved"}}, {Question: Question{ID: "pending"}}}
	got, err := probePendingQuestions(qs, []string{"pending"})
	if err != nil || len(got) != 1 || got[0].ID != "pending" {
		t.Fatalf("resume repeats saved work: %+v %v", got, err)
	}
	for _, ids := range [][]string{{"unknown"}, {"pending", "pending"}, {}} {
		if _, err := probePendingQuestions(qs, ids); err == nil {
			t.Fatalf("unsafe pending selection accepted: %v", ids)
		}
	}
}
