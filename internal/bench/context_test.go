package bench

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/store"
)

func contextWire(t *testing.T, scopePrefix string, seeds, source []Turn, evidence []string) (string, map[string]any, []Row) {
	t.Helper()
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, scoreFixturePool(t), f)
	scope := scopePrefix + sha256Hex([]byte(t.Name()))[:16]
	t.Cleanup(func() {
		if _, err := d.Pool.Exec(context.Background(), `DELETE FROM memory WHERE scope=$1`, scope); err != nil {
			t.Error(err)
		}
	})
	var drafts []draft
	for _, v := range seeds {
		drafts = append(drafts, draft{Content: v.Content, DiaID: v.DiaID, When: v.When})
	}
	if err := ingest(context.Background(), d, scope, drafts); err != nil {
		t.Fatal(err)
	}
	c, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "context-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	d.ScoreCache = c
	d.AnswerOptions = AnswerOptions{ContextPolicy: "neighbors-v1", CaptureInputs: true}
	bySession := map[string][]Turn{}
	for _, v := range source {
		bySession[sessionOfDia(v.DiaID)] = append(bySession[sessionOfDia(v.DiaID)], v)
	}
	// Test the actual scoring loop before the optional index field exists.
	b, _ := json.Marshal(map[string]any{"LoCoMoIndex": map[string]any{scope: bySession}})
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	q := scoredQ{Scope: scope, Question: Question{ID: "context-q", Text: "What chosen color blue?", Type: "4", Answer: "gold-private-sentinel", Evidence: evidence, When: seeds[0].When}}
	var user string
	f.inspectChat = func(model string, msgs []chat.Message) {
		if model != "fixture-reader" {
			return
		}
		user = msgs[1].Content
		state, _ := scoreInputsWire(t, scoreQuestionRecordPath(t, c.dir))
		if state["inputs"].(map[string]any)["reader_user"] != user {
			t.Error("expanded snapshot does not match reader wire")
		}
	}
	rows, err := scoreQuestions(context.Background(), d, []scoredQ{q}, true, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := scoreInputsWire(t, scoreQuestionRecordPath(t, c.dir))
	return user, state, rows
}

func contextTurns() []Turn {
	when := time.Date(2023, 5, 8, 0, 0, 0, 0, time.UTC)
	return []Turn{{DiaID: "D8:4", Content: "previous-turn", When: when}, {DiaID: "D8:5", Content: "seed-choice-question", When: when}, {DiaID: "D8:6", Content: "adjacent-answer-turn", When: when}, {DiaID: "D9:1", Content: "other-session-secret", When: when}}
}

func TestReaderContextAddsAdjacentAnswer(t *testing.T) {
	ts := contextTurns()
	u, _, _ := contextWire(t, "project:bench-locomo-", ts[1:2], ts, []string{"D8:5"})
	if !strings.Contains(u, "previous-turn") || !strings.Contains(u, "adjacent-answer-turn") || strings.Index(u, "previous-turn") > strings.Index(u, "seed-choice-question") {
		t.Fatal("ranked seed's same-session neighbors were not ordered into reader context")
	}
}

func TestReaderContextDoesNotCrossScopeOrSession(t *testing.T) {
	ts := contextTurns()
	u, _, _ := contextWire(t, "project:bench-locomo-", ts[2:3], ts, []string{"D8:6"})
	if !strings.Contains(u, "seed-choice-question") || strings.Contains(u, "other-session-secret") {
		t.Fatal("neighbor expansion missed prior turn or crossed session boundary")
	}
}

func TestReaderContextDoesNotMixRawIntoExtractedOrLME(t *testing.T) {
	for _, prefix := range []string{"project:bench-locomo-x-", "project:bench-lme-"} {
		t.Run(prefix, func(t *testing.T) {
			ts := contextTurns()
			u, state, _ := contextWire(t, prefix, ts[1:2], ts, []string{"D8:5"})
			if strings.Contains(u, "adjacent-answer-turn") || state["inputs"].(map[string]any)["context_policy"] != "" {
				t.Fatal("raw context policy mixed into another arm or snapshot misreported effective policy")
			}
		})
	}
}

func TestReaderContextDeduplicates(t *testing.T) {
	ts := contextTurns()
	u, _, _ := contextWire(t, "project:bench-locomo-", ts[1:3], ts, []string{"D8:5"})
	if strings.Count(u, "adjacent-answer-turn") != 1 || !strings.Contains(u, "previous-turn") {
		t.Fatal("overlapping neighbor windows duplicated/missed turns")
	}
}

func TestReaderContextUTF8(t *testing.T) {
	ts := contextTurns()
	ts[2].Content = "인접 답변 🎨 파란색"
	u, _, _ := contextWire(t, "project:bench-locomo-", ts[1:2], ts, []string{"D8:5"})
	if !utf8.ValidString(u) || !strings.Contains(u, ts[2].Content) {
		t.Fatal("neighbor UTF-8 content lost or corrupted")
	}
}

func TestReaderContextIndependentOfGoldEvidence(t *testing.T) {
	ts := contextTurns()
	u, _, _ := contextWire(t, "project:bench-locomo-", ts[1:2], ts, []string{"evidence-private-sentinel"})
	if strings.Contains(u, "private-sentinel") || !strings.Contains(u, "adjacent-answer-turn") {
		t.Fatal("context construction used labels or omitted neighbors")
	}
}

func TestContextRecallSeparateFromSeedRecallAndLegacyChecksum(t *testing.T) {
	ts := contextTurns()
	_, state, rows := contextWire(t, "project:bench-locomo-", ts[1:2], ts, []string{"D8:6"})
	metrics := state["reader"].(map[string]any)["metrics"].(map[string]any)
	if metrics["recall10"] != float64(0) || metrics["reader_context_recall"] != float64(1) || rows[0].Recall10 != 0 {
		t.Fatalf("expanded coverage was missing or passed off as seed recall: %+v", metrics)
	}
	inputs := state["inputs"].(map[string]any)
	if !reflect.DeepEqual(inputs["context_ids"], []any{"D8:4", "D8:5", "D8:6"}) {
		t.Fatalf("actual context IDs are not in payload order: %v", inputs["context_ids"])
	}
	TestLegacyScoreRecordWithoutInputs(t)
}

func TestReaderContextPreservesSeedsUnderByteBudget(t *testing.T) {
	scope := "project:bench-locomo-unit"
	ts := contextTurns()[:3]
	index := LoCoMoTurnIndex{scope: {"D8": ts}}
	hit := store.Scored{Memory: store.Memory{ID: "seed", Scope: scope, Content: ts[1].Content, CreatedAt: ts[1].When, Attrs: map[string]any{"bench_dia": "D8:5"}}, Score: 1}
	out, err := buildLoCoMoReaderContext(scope, []store.Scored{hit}, index, 1, len(hit.Content))
	if err != nil || !reflect.DeepEqual(out.Contents, []string{hit.Content}) || len(out.NeighborIDs) != 0 {
		t.Fatalf("seed was lost to neighbors: %+v %v", out, err)
	}
	if _, err := buildLoCoMoReaderContext(scope, []store.Scored{hit}, index, 1, len(hit.Content)-1); err == nil {
		t.Fatal("oversize seed silently clipped")
	}
	hit.Scope = "project:other"
	if _, err := buildLoCoMoReaderContext(scope, []store.Scored{hit}, index, 1, 20000); err == nil {
		t.Fatal("another-scope seed accepted")
	}
}

func TestReaderContextSourceIndexRejectsDuplicateIDs(t *testing.T) {
	ts := contextTurns()
	ts = append(ts, ts[1])
	if _, err := newLoCoMoTurnIndex([]LoCoMoSample{{ID: "unit", Turns: ts}}); err == nil {
		t.Fatal("ambiguous source index accepted")
	}
}

func TestContextRecallMustMatchCapturedIDs(t *testing.T) {
	r := 1.0
	s := scoreState{Key: scoreKey{ContextPolicy: "neighbors-v1", CaptureInputs: true, Evidence: []string{"D8:6"}}, Phase: scoreReaderDone,
		Reader: &scoreReader{Metrics: scoreMetrics{ContextRecall: &r}}, Inputs: &scoreInputSnapshot{Version: 1, ReaderSystem: "system", ReaderUser: "user", ContextPolicy: "neighbors-v1", ContextIDs: []string{"D8:5"}}}
	if err := validateScoreState(s); err == nil {
		t.Fatal("context coverage disagrees with actual captured context")
	}
}

func TestContextRecallMarkdownIsSeparateAndOptional(t *testing.T) {
	rep := Report{Sets: []SetResult{{Name: "raw", Rows: []Row{{Label: "all", N: 1, HasF1: true, RecallN: 1, HasContext: true, ContextRecall: 1, ContextN: 1}}}}}
	md := rep.Markdown()
	if !strings.Contains(md, "reader context recall") || !strings.Contains(md, "| 0.000 | 0.000 | 1.000 | 0 |") {
		t.Fatal("expanded coverage not rendered separately from seed recall")
	}
	rep.Sets[0].Rows[0].HasContext = false
	if strings.Contains(rep.Markdown(), "reader context recall") {
		t.Fatal("legacy report format changed")
	}
}
