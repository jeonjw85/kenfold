package bench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
)

func readerWire(t *testing.T, policy string, withF1 bool, change func(*Question)) (string, string) {
	t.Helper()
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, scoreFixturePool(t), f)
	qs := scoreFixtureQuestions(t, d)[:1]
	if change != nil {
		change(&qs[0].Question)
	}
	c, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "reader-v2")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d.ScoreCache = c
	d.AnswerOptions = AnswerOptions{ReaderPolicy: policy, CaptureInputs: true}
	var system, user string
	f.inspectChat = func(model string, msgs []chat.Message) {
		if model != "fixture-reader" {
			return
		}
		system, user = msgs[0].Content, msgs[1].Content
		state, _ := scoreInputsWire(t, scoreQuestionRecordPath(t, c.dir))
		inputs := state["inputs"].(map[string]any)
		if inputs["reader_system"] != system || inputs["reader_user"] != user {
			t.Error("snapshot differs from actual HTTP reader payload")
		}
	}
	if _, err := scoreQuestions(context.Background(), d, qs, withF1, t.Logf); err != nil {
		t.Fatal(err)
	}
	return system, user
}

func TestReaderLegacyMessagesUnchanged(t *testing.T) {
	s, u := readerWire(t, "", true, nil)
	if s != readerSystem || u != "Memories:\n1. The chosen color is blue.\n\nQuestion: What color?" {
		t.Fatal("legacy payload changed")
	}
}

func TestReaderTemporalUsesMemoryDate(t *testing.T) {
	s, _ := readerWire(t, "grounded-v2", true, nil)
	if !strings.Contains(s, "Use DATE of CONVERSATION to answer with an approximate date.") || !strings.Contains(s, "relative to the date of that memory") {
		t.Fatal("missing conversation-relative date instructions")
	}
}

func TestReaderLoCoMoPresentNotQuestionDate(t *testing.T) {
	_, u := readerWire(t, "grounded-v2", true, func(q *Question) { q.When = time.Date(2077, 7, 1, 0, 0, 0, 0, time.UTC) })
	if strings.Contains(u, "2077") || strings.Contains(u, "Question date:") {
		t.Fatal("synthetic recency Present leaked as question date")
	}
}

func TestReaderLMEDateIsExplicit(t *testing.T) {
	_, u := readerWire(t, "grounded-v2", false, func(q *Question) { q.Type = "temporal-reasoning" })
	if !strings.Contains(u, "Question date: 2023-05-08T13:56:00Z") {
		t.Fatal("actual LME question date was omitted")
	}
}

func TestReaderOpenDomainGroundedInference(t *testing.T) {
	for _, typ := range []string{"1", "3", "4"} {
		t.Run(typ, func(t *testing.T) {
			s, _ := readerWire(t, "grounded-v2", true, func(q *Question) { q.Type = typ })
			if strings.Contains(s, "general knowledge") != (typ == "3") || !strings.Contains(s, "Do not invent personal facts") {
				t.Fatal("open-domain inference is not narrowly grounded")
			}
		})
	}
}

func TestReaderDoesNotExposeGoldEvidence(t *testing.T) {
	s, u := readerWire(t, "grounded-v2", true, func(q *Question) {
		q.Answer = "gold-private-sentinel"
		q.Evidence = []string{"evidence-private-sentinel"}
	})
	if strings.Contains(s+u, "private-sentinel") {
		t.Fatal("labels leaked into reader payload")
	}
}

func TestNewReaderPoliciesRequireSnapshotCacheBeforeNetwork(t *testing.T) {
	for _, opts := range []AnswerOptions{{ReaderPolicy: "grounded-v2"}, {ReaderPolicy: "grounded-v2", CaptureInputs: true}, {CaptureInputs: true}, {ReaderPolicy: "mistyped"}, {ContextPolicy: "mistyped"}} {
		f := newScoreHTTPFixture(t)
		d := scoreFixtureDeps(t, scoreFixturePool(t), f)
		qs := scoreFixtureQuestions(t, d)[:1]
		d.AnswerOptions = opts
		e, r, j := f.embedding.Load(), f.reader.Load(), f.judge.Load()
		if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err == nil {
			t.Errorf("options %+v reached network without required cache/snapshot", opts)
		}
		if f.embedding.Load() != e || f.reader.Load() != r || f.judge.Load() != j {
			t.Errorf("invalid options %+v made calls", opts)
		}
	}
}

func TestReaderOptionsRejectedBeforeStartup(t *testing.T) {
	for _, name := range []string{"KENFOLD_BENCH_READER_POLICY", "KENFOLD_BENCH_CONTEXT_POLICY", "KENFOLD_BENCH_CAPTURE_INPUTS"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestBench$", "-test.v")
		cmd.Env = append(scoreChildEnv(), "KENFOLD_TEST_DATABASE_URL=postgres://127.0.0.1:1/never", "KENFOLD_EVAL_EMBED_URL=http://127.0.0.1:1", "KENFOLD_BENCH_CHAT_URL=http://127.0.0.1:1", name+"=mistyped")
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "invalid "+name) {
			t.Errorf("%s was not rejected before startup: %s", name, out)
		}
	}
}

func TestReaderOptionsNormalization(t *testing.T) {
	for _, v := range []struct {
		reader, context, capture string
		want                     AnswerOptions
	}{
		{"", "", "", AnswerOptions{}},
		{"legacy", "turns", "0", AnswerOptions{}},
		{"grounded-v2", "neighbors-v1", "1", AnswerOptions{ReaderPolicy: "grounded-v2", ContextPolicy: "neighbors-v1", CaptureInputs: true}},
	} {
		t.Setenv("KENFOLD_BENCH_READER_POLICY", v.reader)
		t.Setenv("KENFOLD_BENCH_CONTEXT_POLICY", v.context)
		t.Setenv("KENFOLD_BENCH_CAPTURE_INPUTS", v.capture)
		got, err := answerOptionsFromEnv()
		if err != nil || got != v.want {
			t.Fatalf("normalization: %+v, %v", got, err)
		}
	}
}

func TestReaderOptionsRunRejectsBeforeDBOrHealth(t *testing.T) {
	// Nil dependencies would panic if Run touched DB/models before validation.
	if _, err := Run(context.Background(), Deps{AnswerOptions: AnswerOptions{ReaderPolicy: "grounded-v2"}}, Config{}); err == nil {
		t.Fatal("invalid new-policy configuration accepted")
	}
}
