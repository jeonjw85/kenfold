package bench

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/memory"
)

// Restarting before sample ingestion used to repeat every completed session.
// Exercise the real extractor/validator and stop at the embedding boundary,
// without a database or a model service.
func TestExtractionCheckpointRestartBeforeIngestion(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	ex := &checkpointChat{model: "fixture", replies: []string{
		`{"items":[]}`,
		`{"items":[{"category":"project_rule","content":"Use pnpm in this project.","evidence":"Use pnpm in this project.","basis":"stated"},{"category":"code_fact","content":"The cache lives in cache.go.","evidence":"The cache lives in cache.go.","basis":"inferred"}]}`,
	}, failAt: 3}
	samples := checkpointSamples()
	d := Deps{Extractor: ex, Embedder: &checkpointEmbedder{err: errors.New("stop before DB")}}
	cache, err := NewExtractionCache(dir, "pinned-v1")
	if err != nil {
		t.Fatal(err)
	}
	d.ExtractCache = cache
	if err := extractLoCoMo(context.Background(), d, samples, t.Logf); err == nil || !strings.Contains(err.Error(), "fixture interruption") {
		t.Fatalf("want interruption in third session, got %v", err)
	}
	if ex.calls != 3 {
		t.Fatalf("first run made %d extraction calls, want 3", ex.calls)
	}

	// A new cache object and new run dependencies model a process restart and
	// an empty database. The previously failed session must still be extracted.
	cache, err = NewExtractionCache(dir, "pinned-v1")
	if err != nil {
		t.Fatal(err)
	}
	restarted := &checkpointChat{model: "fixture", replies: []string{`{"items":[]}`}}
	emb := &checkpointEmbedder{err: errors.New("stop before DB")}
	d = Deps{Extractor: restarted, Embedder: emb}
	d.ExtractCache = cache
	if err := extractLoCoMo(context.Background(), d, samples, t.Logf); err == nil || !strings.Contains(err.Error(), "stop before DB") {
		t.Fatalf("want normal ingestion to reach embedding, got %v", err)
	}
	if restarted.calls != 1 {
		t.Fatalf("restart repeated completed sessions: %d extraction calls, want only 1 for failed session", restarted.calls)
	}
	want := []string{"Use pnpm in this project.", "The cache lives in cache.go."}
	if !reflect.DeepEqual(emb.texts, want) {
		t.Fatalf("replayed ingestion projection = %q, want %q", emb.texts, want)
	}

	// Once all sessions are durable, even successful empty results are hits.
	cache, err = NewExtractionCache(dir, "pinned-v1")
	if err != nil {
		t.Fatal(err)
	}
	replayed := &checkpointChat{model: "fixture", failAt: 1}
	d.Extractor = replayed
	d.ExtractCache = cache
	_ = extractLoCoMo(context.Background(), d, samples, t.Logf)
	if replayed.calls != 0 {
		t.Fatalf("fully cached sample made %d new extraction calls", replayed.calls)
	}
}

func checkpointSamples() []LoCoMoSample {
	when := time.Date(2023, 5, 8, 13, 56, 0, 0, time.UTC)
	return []LoCoMoSample{{ID: "fixture", Turns: []Turn{
		{DiaID: "D1:1", Content: "Nothing durable here.", When: when},
		{DiaID: "D2:1", Content: "Use pnpm in this project.", When: when.Add(time.Hour)},
		{DiaID: "D2:2", Content: "The cache lives in cache.go.", When: when.Add(time.Hour)},
		{DiaID: "D3:1", Content: "Last session.", When: when.Add(2 * time.Hour)},
	}}}
}

type checkpointChat struct {
	model   string
	replies []string
	failAt  int
	calls   int
	before  func()
}

func (c *checkpointChat) Model() string { return c.model }
func (c *checkpointChat) JSON(_ context.Context, req chat.Request, out any) (string, chat.Usage, error) {
	c.calls++
	if c.before != nil {
		c.before()
	}
	if c.calls == c.failAt {
		return "", chat.Usage{}, errors.New("fixture interruption")
	}
	if req.SchemaName != "memories" || req.MaxTokens != 1200 {
		return "", chat.Usage{}, errors.New("unexpected extraction request")
	}
	if c.calls > len(c.replies) {
		return "", chat.Usage{}, errors.New("unexpected extra extraction")
	}
	raw := c.replies[c.calls-1]
	return raw, chat.Usage{}, json.Unmarshal([]byte(raw), out)
}

type checkpointEmbedder struct {
	texts []string
	err   error
}

func (e *checkpointEmbedder) Model() string { return "fixture" }
func (e *checkpointEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.texts = append([]string(nil), texts...)
	return nil, e.err
}

func TestExtractionCheckpointNamespaceAndProjection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	c, err := NewExtractionCache(dir, "pinned-v1")
	if err != nil {
		t.Fatal(err)
	}
	key := c.Key("fixture", "D1", "fixture-model", "abc")
	if key.SourceSHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("source hash does not cover exact UTF-8 bytes: %s", key.SourceSHA256)
	}
	want := []ExtractionProjection{
		{Content: "Use pnpm in this project.", Type: memory.TypeProject, Confidence: 0.9},
		{Content: "The cache lives in cache.go.", Type: memory.TypeCodebase, Confidence: 0.6},
	}
	if err := c.Save(key, want); err != nil {
		t.Fatal(err)
	}
	got, hit, err := c.Load(key)
	if err != nil || !hit || !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered content/type/confidence changed: %+v, hit=%v, err=%v", got, hit, err)
	}
	other, err := NewExtractionCache(dir, "pinned-v2")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		c    *ExtractionCache
		key  ExtractionKey
	}{
		{"source", c, c.Key("fixture", "D1", "fixture-model", "abc\n")},
		{"model", c, c.Key("fixture", "D1", "other-model", "abc")},
		{"identity", other, other.Key("fixture", "D1", "fixture-model", "abc")},
		{"sample", c, c.Key("other-sample", "D1", "fixture-model", "abc")},
		{"session", c, c.Key("fixture", "D2", "fixture-model", "abc")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, hit, err := tc.c.Load(tc.key); err != nil || hit {
				t.Fatalf("namespace change reused or collided with stale output: hit=%v err=%v", hit, err)
			}
		})
	}
	// Even a seeded successful empty list is an explicit, replayable record.
	empty := c.Key("fixture", "D3", "fixture-model", "empty")
	if err := c.Save(empty, nil); err != nil {
		t.Fatal(err)
	}
	if got, hit, err := c.Load(empty); err != nil || !hit || got == nil || len(got) != 0 {
		t.Fatalf("empty success not preserved as []: %v %v %v", got, hit, err)
	}
	path, err := c.Path(key)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("checkpoint must be owner-only: %v %v", info, err)
	}
	if err := c.Save(key, []ExtractionProjection{{Content: "Different projection.", Type: memory.TypeSemantic, Confidence: 0.9}}); err == nil {
		t.Fatal("existing projection was silently replaced")
	}
}

func TestExtractionCheckpointCorruptRecordNeverRecomputes(t *testing.T) {
	for _, corruption := range []string{"json", "key", "null", "projection", "unknown", "trailing", "public", "symlink"} {
		t.Run(corruption, func(t *testing.T) {
			c, err := NewExtractionCache(filepath.Join(t.TempDir(), "cache"), "pinned-v1")
			if err != nil {
				t.Fatal(err)
			}
			samples := checkpointSamples()
			key := c.Key("fixture", "D1", "fixture", "Nothing durable here.\n")
			if err := c.Save(key, nil); err != nil {
				t.Fatal(err)
			}
			path, err := c.Path(key)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var rec extractionRecord
			if err := json.Unmarshal(data, &rec); err != nil {
				t.Fatal(err)
			}
			switch corruption {
			case "json":
				data = []byte("{")
			case "key":
				rec.Key.Sample = "different"
				data, err = json.Marshal(rec)
			case "null":
				rec.Memories = nil
				data, err = json.Marshal(rec)
			case "projection":
				rec.Memories = []ExtractionProjection{{Content: "Invented corruption.", Type: memory.TypeProject, Confidence: 0.9}}
				data, err = json.Marshal(rec)
			case "unknown":
				data = append([]byte(`{"raw":"must not accept",`), data[1:]...)
			case "trailing":
				data = append(data, []byte(`{}`)...)
			case "public":
				err = os.Chmod(path, 0o644)
			case "symlink":
				backup := path + ".backup"
				if err = os.Rename(path, backup); err == nil {
					err = os.Symlink(backup, path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if corruption != "public" && corruption != "symlink" {
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ex := &checkpointChat{model: "fixture", failAt: 1}
			logs := 0
			if err := extractLoCoMo(context.Background(), Deps{Extractor: ex, ExtractCache: c}, samples, func(string, ...any) { logs++ }); err == nil || !strings.Contains(err.Error(), "checkpoint") {
				t.Fatalf("corrupt record not reported clearly: %v", err)
			}
			if ex.calls != 0 || logs != 0 {
				t.Fatalf("corrupt record was recomputed or logged successful: calls=%d logs=%d", ex.calls, logs)
			}
			if err := c.Save(key, nil); err == nil {
				t.Fatal("seed overwrote corrupt record")
			}
		})
	}
}

func TestExtractionCheckpointPublicationFailureNotSuccessful(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	c, err := NewExtractionCache(dir, "pinned-v1")
	if err != nil {
		t.Fatal(err)
	}
	ex := &checkpointChat{model: "fixture", replies: []string{`{"items":[]}`}, before: func() {
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
	}}
	samples := checkpointSamples()
	samples[0].Turns = samples[0].Turns[:1]
	logs := 0
	if err := extractLoCoMo(context.Background(), Deps{Extractor: ex, ExtractCache: c}, samples, func(string, ...any) { logs++ }); err == nil || !strings.Contains(err.Error(), "publish extraction checkpoint") {
		t.Fatalf("publication failure was discarded: %v", err)
	}
	if ex.calls != 1 || logs != 0 {
		t.Fatalf("failed checkpoint logged success: calls=%d logs=%d", ex.calls, logs)
	}
	c, err = NewExtractionCache(dir, "pinned-v1")
	if err != nil {
		t.Fatal(err)
	}
	ex = &checkpointChat{model: "fixture", replies: []string{`{"items":[]}`}}
	if err := extractLoCoMo(context.Background(), Deps{Extractor: ex, ExtractCache: c}, samples, t.Logf); err != nil {
		t.Fatal(err)
	}
	if ex.calls != 1 {
		t.Fatalf("failed publication became cached success: calls=%d", ex.calls)
	}
}

func TestExtractionCheckpointValidation(t *testing.T) {
	for _, identity := range []string{"", " \n"} {
		if _, err := NewExtractionCache(filepath.Join(t.TempDir(), "cache"), identity); err == nil {
			t.Fatal("unpinned cache identity accepted")
		}
	}
	c, err := NewExtractionCache(filepath.Join(t.TempDir(), "cache"), "pinned-v1")
	if err != nil {
		t.Fatal(err)
	}
	key := c.Key("fixture", "D1", "fixture", "source")
	for _, p := range []ExtractionProjection{
		{Content: "", Type: memory.TypeProject, Confidence: 0.9},
		{Content: "Invalid type.", Type: "unknown", Confidence: 0.9},
		{Content: "Invalid confidence.", Type: memory.TypeProject, Confidence: math.NaN()},
		{Content: "Invalid confidence.", Type: memory.TypeProject, Confidence: math.Inf(1)},
		{Content: "Invalid confidence.", Type: memory.TypeProject, Confidence: -1},
	} {
		if err := c.Save(key, []ExtractionProjection{p}); err == nil {
			t.Fatalf("invalid projection accepted: %+v", p)
		}
	}
	if _, hit, err := c.Load(key); hit || err != nil {
		t.Fatalf("rejected seed created a cache record: %v %v", hit, err)
	}
}

func TestExtractionCheckpointRejectsFIFOWithoutBlocking(t *testing.T) {
	c, err := NewExtractionCache(filepath.Join(t.TempDir(), "cache"), "pinned-v1")
	if err != nil {
		t.Fatal(err)
	}
	key := c.Key("fixture", "D1", "fixture", "source")
	path, err := c.Path(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Load(key)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("nonregular cache record was accepted")
		}
	case <-time.After(time.Second):
		// Unblock the old implementation for cleanup before reporting RED.
		writer, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		<-done
		t.Fatal("corrupt FIFO checkpoint blocked instead of being rejected")
	}
}

func TestExtractionCheckpointAbruptExit(t *testing.T) {
	if dir := os.Getenv("KENFOLD_CHECKPOINT_CRASH_CACHE_DIR"); dir != "" {
		c, err := NewExtractionCache(dir, "pinned-v1")
		if err != nil {
			t.Fatal(err)
		}
		ex := &checkpointChat{model: "fixture", replies: []string{
			`{"items":[]}`,
			`{"items":[{"category":"project_rule","content":"Use pnpm in this project.","evidence":"Use pnpm in this project.","basis":"stated"}]}`,
		}}
		logs := 0
		_ = extractLoCoMo(context.Background(), Deps{Extractor: ex, ExtractCache: c}, checkpointSamples(), func(string, ...any) {
			logs++
			if logs == 2 {
				os.Exit(29) // No deferred cleanup, before the sample is ingested.
			}
		})
		t.Fatal("fixture did not exit at its second session progress log")
	}
	dir := filepath.Join(t.TempDir(), "cache")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExtractionCheckpointAbruptExit$")
	cmd.Env = append(os.Environ(), "KENFOLD_CHECKPOINT_CRASH_CACHE_DIR="+dir)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 29 {
		t.Fatalf("fixture did not exit abruptly: %v\n%s", err, out)
	}
	c, err := NewExtractionCache(dir, "pinned-v1")
	if err != nil {
		t.Fatal(err)
	}
	ex := &checkpointChat{model: "fixture", replies: []string{`{"items":[]}`}}
	emb := &checkpointEmbedder{err: errors.New("stop before DB")}
	if err := extractLoCoMo(context.Background(), Deps{Extractor: ex, ExtractCache: c, Embedder: emb}, checkpointSamples(), t.Logf); err == nil || !strings.Contains(err.Error(), "stop before DB") {
		t.Fatalf("restart did not replay into ingestion: %v", err)
	}
	if ex.calls != 1 || !reflect.DeepEqual(emb.texts, []string{"Use pnpm in this project."}) {
		t.Fatalf("abrupt exit lost sessions or ingestion: calls=%d texts=%q", ex.calls, emb.texts)
	}
}
