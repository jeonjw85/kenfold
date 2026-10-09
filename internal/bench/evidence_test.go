package bench

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
)

// These fixtures verify emitted and durable reader inputs, not whether a real
// language model follows the instructions or changes its answers correctly.
func TestEvidenceReaderSendsRulesInDurableHTTPPayload(t *testing.T) {
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, scoreFixturePool(t), f)
	qs := scoreFixtureQuestions(t, d)[:1]
	qs[0].Text = "Did Mira recommend a recipe to Sol?"
	qs[0].Answer = "gold-private-sentinel"
	qs[0].Evidence = []string{"evidence-private-sentinel"}
	qs[0].When = time.Date(2077, 1, 1, 0, 0, 0, 0, time.UTC)
	memory := "[8:55 pm on 25 February, 2023, session 7, D7:5] Mira: I taught a cooking class yesterday."
	if _, err := d.Pool.Exec(context.Background(), `UPDATE memory SET content=$1 WHERE scope=$2`, memory, qs[0].Scope); err != nil {
		t.Fatal(err)
	}
	cache, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "evidence-http-fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	d.ScoreCache = cache
	d.AnswerOptions = AnswerOptions{ReaderPolicy: "evidence-v1", CaptureInputs: true}
	f.inspectChat = func(model string, msgs []chat.Message) {
		if model != "fixture-reader" {
			return
		}
		state, _ := scoreInputsWire(t, scoreQuestionRecordPath(t, cache.dir))
		inputs := state["inputs"].(map[string]any)
		if state["phase"] != "reader-in-flight" || inputs["reader_system"] != msgs[0].Content || inputs["reader_user"] != msgs[1].Content || inputs["reader_policy"] != "evidence-v1" {
			t.Error("evidence policy is not the exact durable pre-call HTTP input")
		}
		for _, rule := range []string{"actor, action, recipient", "same event", "Absence of evidence is not evidence of no", "evidence supporting no", "I do not know", "Tentative predictions", "not facts about the people in the memories"} {
			if !strings.Contains(msgs[0].Content, rule) {
				t.Errorf("actual reader request omitted evidence rule %q", rule)
			}
		}
		if !strings.Contains(msgs[1].Content, "1. "+memory+"\n") || !strings.Contains(msgs[1].Content, `"yesterday": 2023-02-24`) {
			t.Error("evidence policy lost original memory/calendar hint")
		}
		if strings.Contains(msgs[0].Content+msgs[1].Content, "private-sentinel") || strings.Contains(msgs[1].Content, "2077") {
			t.Error("question labels or synthetic recency date leaked into reader input")
		}
	}
	if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err != nil {
		t.Fatal(err)
	}
	if f.reader.Load() != 1 || f.judge.Load() != 1 {
		t.Fatal("instruction strengthening added extra model calls")
	}
	d.AnswerOptions.ReaderPolicy = "temporal-v1"
	if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err == nil {
		t.Fatal("new policy silently reused a different reader policy cache slot")
	}
	if f.reader.Load() != 1 || f.judge.Load() != 1 {
		t.Fatal("reader policy drift made additional calls")
	}
}

func TestEvidenceReaderRequiresCaptureAndPrivateCacheBeforeStartup(t *testing.T) {
	t.Setenv("KENFOLD_BENCH_READER_POLICY", "evidence-v1")
	t.Setenv("KENFOLD_BENCH_CAPTURE_INPUTS", "1")
	t.Setenv("KENFOLD_BENCH_CONTEXT_POLICY", "")
	opts, err := answerOptionsFromEnv()
	if err != nil || opts.ReaderPolicy != "evidence-v1" || !opts.CaptureInputs {
		t.Fatalf("explicit evidence policy cannot be selected: %+v %v", opts, err)
	}
	cache, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "evidence-config-fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err := validateAnswerOptions(opts, cache); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []AnswerOptions{{ReaderPolicy: "evidence-v1"}, opts} {
		if _, err := Run(context.Background(), Deps{AnswerOptions: invalid}, Config{}); err == nil {
			t.Fatal("uncaptured/unisolated policy reached startup dependencies")
		}
	}
	opts.CaptureInputs = false
	if err := validateAnswerOptions(opts, cache); err == nil {
		t.Fatal("private cache alone allowed uncaptured evidence-policy inputs")
	}
}

func TestEvidenceReaderPreservesLegacyAndDatasetDateSemantics(t *testing.T) {
	memory := "[8:55 pm on 25 February, 2023, session 7, D7:5] Mira: We travelled yesterday."
	q := Question{Text: "When?", Type: "2"}
	for _, policy := range []string{"", "grounded-v2", "temporal-v1"} {
		system, _, err := readerMessages(q, []string{memory}, true, policy)
		if err != nil || strings.Contains(system, "Absence of evidence") {
			t.Fatal("experimental evidence rules leaked into existing policies")
		}
	}
	for _, withF1 := range []bool{true, false} {
		q.Type = "3"
		q.When = time.Date(2023, 3, 1, 0, 0, 0, 0, time.UTC)
		system, user, err := readerMessages(q, []string{memory}, withF1, "evidence-v1")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(user, "Calendar hints:") != withF1 || strings.HasPrefix(user, "Question date: 2023-03-01T00:00:00Z") == withF1 {
			t.Fatal("evidence policy changed LoCoMo/LME date handling")
		}
		if withF1 && !strings.Contains(system, "general knowledge to infer an answer anchored in the memories") {
			t.Fatal("evidence strengthening removed allowed anchored open-domain inference")
		}
	}
}

func TestEvidenceReaderDoesNotBroadenGeneralKnowledgePermission(t *testing.T) {
	for _, typ := range []string{"1", "2", "3", "4", "temporal-reasoning"} {
		for _, withF1 := range []bool{true, false} {
			system, _, err := readerMessages(Question{Text: "What might happen?", Type: typ}, nil, withF1, "evidence-v1")
			if err != nil || strings.Contains(system, "general knowledge") != (withF1 && typ == "3") {
				t.Fatalf("evidence rules broadened inference beyond original dataset/category permission: %s %t %v", typ, withF1, err)
			}
		}
	}
}
