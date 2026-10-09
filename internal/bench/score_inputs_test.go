package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kenfold/kenfold/internal/chat"
)

// JSON config keeps these behavioral tests runnable before AnswerOptions exists.
func captureScoreInputs(t *testing.T, d *Deps) {
	t.Helper()
	if err := json.Unmarshal([]byte(`{"AnswerOptions":{"CaptureInputs":true}}`), d); err != nil {
		t.Fatal(err)
	}
}

func scoreInputsWire(t *testing.T, path string) (map[string]any, []byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	return wire["state"].(map[string]any), b
}

func TestScoreInputsPersistBeforeReaderCall(t *testing.T) {
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, scoreFixturePool(t), f)
	qs := scoreFixtureQuestions(t, d)[:1]
	c, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "inputs-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d.ScoreCache = c
	captureScoreInputs(t, &d)
	seen := false
	f.beforeChat = func(model string) {
		if model != "fixture-reader" {
			return
		}
		seen = true
		state, _ := scoreInputsWire(t, scoreQuestionRecordPath(t, c.dir))
		inputs, ok := state["inputs"].(map[string]any)
		if !ok {
			t.Error("reader reached network without durable input snapshot")
			return
		}
		if state["phase"] != "reader-in-flight" || inputs["reader_system"] != readerSystem || inputs["reader_user"] != "Memories:\n1. The chosen color is blue.\n\nQuestion: What color?" {
			t.Errorf("persisted input is not the actual reader payload: %+v", inputs)
		}
		hits := inputs["hits"].([]any)
		if len(hits) != 1 || hits[0].(map[string]any)["content"] != "The chosen color is blue." || hits[0].(map[string]any)["scope"] != qs[0].Scope || !reflect.DeepEqual(inputs["context_ids"], []any{"D1:1"}) {
			t.Errorf("input snapshot lost ordered retrieval provenance: %+v", inputs)
		}
	}
	if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("real scoring loop did not call the reader")
	}
}

func TestScoreInputsImmutableOnReplay(t *testing.T) {
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, scoreFixturePool(t), f)
	qs := scoreFixtureQuestions(t, d)[:1]
	c, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "inputs-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d.ScoreCache = c
	captureScoreInputs(t, &d)
	rows, err := scoreQuestions(context.Background(), d, qs, true, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	path := scoreQuestionRecordPath(t, c.dir)
	state, before := scoreInputsWire(t, path)
	if state["inputs"] == nil {
		t.Fatal("completed answer has no replayable input snapshot")
	}
	reader, judge, embedding := f.reader.Load(), f.judge.Load(), f.embedding.Load()
	replay, err := scoreQuestions(context.Background(), d, qs, true, t.Logf)
	_, after := scoreInputsWire(t, path)
	if err != nil || !reflect.DeepEqual(rows, replay) || string(before) != string(after) || f.reader.Load() != reader || f.judge.Load() != judge || f.embedding.Load() != embedding {
		t.Fatalf("replay changed saved input or repeated calls: %v", err)
	}
}

func TestPolicyKeyMismatchStopsCalls(t *testing.T) {
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, scoreFixturePool(t), f)
	qs := scoreFixtureQuestions(t, d)[:1]
	c, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "inputs-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d.ScoreCache = c
	if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err != nil {
		t.Fatal(err)
	}
	reader, judge, embedding := f.reader.Load(), f.judge.Load(), f.embedding.Load()
	captureScoreInputs(t, &d)
	if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("changed input policy silently reused old results: %v", err)
	}
	if f.reader.Load() != reader || f.judge.Load() != judge || f.embedding.Load() != embedding {
		t.Fatal("incompatible policy reached retrieval or completions")
	}
}

func TestLegacyScoreRecordWithoutInputs(t *testing.T) {
	c, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "legacy-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	client, err := chat.New(chat.Config{BaseURL: "http://127.0.0.1:1", Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	key := c.key(scoredQ{Scope: "project:legacy", Question: Question{ID: "q", Text: "Color?", Answer: "blue", Evidence: []string{"D1:1"}}}, true, client, client)
	k, _ := json.Marshal(key)
	for _, field := range []string{"reader_policy", "context_policy", "capture_inputs"} {
		if strings.Contains(string(k), field) {
			t.Fatalf("zero options changed the legacy wire key: %s", field)
		}
	}
	// Original v1 field order and bytes, independent of new optional structs.
	s := fmt.Sprintf(`{"key":%s,"phase":"scored","reader":{"answer":"blue","metrics":{"f1":1,"recall5":1,"recall10":1}},"judge":{"text":"yes","yes":true}}`, k)
	record := fmt.Sprintf(`{"state":%s,"state_sha256":"%s"}`, s, sha256Hex([]byte(s)))
	path, err := c.path(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	state, hit, err := c.load(key)
	after, _ := os.ReadFile(path)
	if err != nil || !hit || state.Reader.Answer != "blue" || string(after) != record {
		t.Fatalf("old record was rejected or rewritten: %v", err)
	}
}

type failedBenchEmbedding struct{}

func (failedBenchEmbedding) Model() string { return "fixture-embedding" }
func (failedBenchEmbedding) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("https://user:private-secret@example.invalid/v1?token=private-secret")
}

func TestBenchmarkSearchDiagnosticsAreSanitized(t *testing.T) {
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, scoreFixturePool(t), f)
	qs := scoreFixtureQuestions(t, d)[:1]
	c, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "diagnostics-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d.ScoreCache, d.Embedder = c, failedBenchEmbedding{}
	captureScoreInputs(t, &d)
	var log strings.Builder
	if _, err := scoreQuestions(context.Background(), d, qs, true, func(format string, args ...any) { fmt.Fprintf(&log, format, args...) }); err != nil {
		t.Fatal(err)
	}
	got := log.String()
	if !strings.Contains(got, `"stage":"embedding"`) || !strings.Contains(got, `"degraded":true`) {
		t.Fatalf("search degradation was not observable: %s", got)
	}
	for _, secret := range []string{"private-secret", "example.invalid", "https://", "The chosen color"} {
		if strings.Contains(got, secret) {
			t.Fatalf("diagnostic leaked private data: %s", secret)
		}
	}
}
