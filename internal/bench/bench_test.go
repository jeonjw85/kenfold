package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestClipBytes(t *testing.T) {
	s := strings.Repeat("a", 2500) + "한"
	got := clipBytes(s, maxContentBytes)
	if len(got) > maxContentBytes || !utf8.ValidString(got) {
		t.Fatalf("clip = %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}

func TestDates(t *testing.T) {
	loc, err := parseLoCoMoDate("1:56 pm on 8 May, 2023")
	if err != nil || loc.Year() != 2023 || loc.Month() != time.May || loc.Day() != 8 || loc.Hour() != 13 || loc.Minute() != 56 {
		t.Fatalf("locomo date = %v, %v", loc, err)
	}
	lme, err := parseLMEDate("2023/05/30 (Tue) 23:40")
	if err != nil || lme.Format("2006-01-02 15:04") != "2023-05-30 23:40" {
		t.Fatalf("lme date = %v, %v", lme, err)
	}
}

func TestLoCoMoReleasedCategoryLabels(t *testing.T) {
	// Released dataset IDs differ from the numbered list in the paper.
	// See snap-research/locomo task_eval/evaluation.py and issues/6.
	for _, tc := range []struct{ id, label string }{
		{"1", "1 multi-hop"},
		{"2", "2 temporal"},
		{"3", "3 open-domain"},
		{"4", "4 single-hop"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if got := LoCoMoCategory(tc.id); got != tc.label {
				t.Fatalf("released category %s mislabeled: %q, want %q", tc.id, got, tc.label)
			}
		})
	}
}

func TestTokenF1(t *testing.T) {
	if f1Score("The cats!", "cats") != 1 {
		t.Fatalf("articles and punct: %v", f1Score("The cats!", "cats"))
	}
	if f1Score("1,000", "1000") != 1 {
		t.Fatalf("comma: %v", f1Score("1,000", "1000"))
	}
	if got := TokenF1("blue, red", "red, blue", "1"); got != 1 {
		t.Fatalf("category 1 = %v", got)
	}
	if got := TokenF1("red", "red, blue", "1"); got != 0.5 {
		t.Fatalf("partial category 1 = %v", got)
	}
	if f1Score("running", "run") != 0 {
		t.Fatal("unstemmed F1 should not stem")
	}
}

func TestJudge(t *testing.T) {
	q := Question{Type: "temporal-reasoning", Text: "when", Answer: "18 days"}
	p := JudgePrompt(q, "19 days")
	if !strings.Contains(p, "off-by-one") || !strings.Contains(p, "19 days") {
		t.Fatalf("temporal prompt = %q", p)
	}
	abs := JudgePrompt(Question{Abstain: true, Text: "hamster?", Answer: "not mentioned"}, "I don't know")
	if !strings.Contains(abs, "unanswerable") {
		t.Fatalf("abstain prompt = %q", abs)
	}
	if !JudgeYes("Yes.") || !JudgeYes("yes, it does") || JudgeYes("No") {
		t.Fatal("JudgeYes")
	}
}

func TestEvidenceRecall(t *testing.T) {
	if evidenceRecall(nil, []string{"D1:1"}) != -1 {
		t.Fatal("no evidence")
	}
	if got := evidenceRecall([]string{"D1:3", "D2:1"}, []string{"D1:3", "D9:1"}); got != 0.5 {
		t.Fatalf("dia recall = %v", got)
	}
	if got := evidenceRecall([]string{"D1:3"}, []string{"D1"}); got != 1 {
		t.Fatalf("session covers turn = %v", got)
	}
	if got := evidenceRecall([]string{"D1:3"}, []string{"D1:1"}); got != 0 {
		t.Fatalf("other turn = %v", got)
	}
}

func TestSubset(t *testing.T) {
	counts := map[string]int{
		"multi-session": 133, "temporal-reasoning": 133, "knowledge-update": 78,
		"single-session-user": 70, "single-session-assistant": 56, "single-session-preference": 30,
	}
	var all []LMEQuestion
	for typ, n := range counts {
		for i := 0; i < n; i++ {
			all = append(all, LMEQuestion{Question: Question{ID: fmt.Sprintf("%s-%03d", typ, i), Type: typ}})
		}
	}
	got := subsetLME(all, 60)
	if len(got) != 60 {
		t.Fatalf("subset = %d", len(got))
	}
	c := map[string]int{}
	for _, q := range got {
		c[q.Type]++
	}
	want := map[string]int{
		"multi-session": 16, "temporal-reasoning": 16, "knowledge-update": 9,
		"single-session-user": 8, "single-session-assistant": 7, "single-session-preference": 4,
	}
	for k, n := range want {
		if c[k] != n {
			t.Errorf("%s = %d, want %d", k, c[k], n)
		}
	}
	again := subsetLME(all, 60)
	if again[0].ID != got[0].ID {
		t.Fatal("subset is not stable")
	}
}

func TestLoadFixtures(t *testing.T) {
	dir := t.TempDir()
	locomo := `[{
		"sample_id": "conv-1",
		"qa": [
			{"question": "When?", "answer": "7 May 2023", "evidence": ["D1:1"], "category": 2},
			{"question": "fields?", "answer": "Psychology; counseling", "evidence": ["D1:2"], "category": 3},
			{"question": "trick", "answer": null, "evidence": ["D1:1"], "category": 5}
		],
		"conversation": {
			"speaker_a": "Caroline",
			"session_1_date_time": "1:56 pm on 8 May, 2023",
			"session_1": [
				{"speaker": "Caroline", "dia_id": "D1:1", "text": "I went on 7 May.", "blip_caption": "a photo of a group"},
				{"speaker": "Mel", "dia_id": "D1:2", "text": "Nice."}
			]
		}
	}]`
	if err := os.WriteFile(filepath.Join(dir, "locomo10.json"), []byte(locomo), 0o644); err != nil {
		t.Fatal(err)
	}
	samples, err := LoadLoCoMo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || len(samples[0].Questions) != 2 || len(samples[0].Turns) != 2 {
		t.Fatalf("sample = %+v", samples)
	}
	if samples[0].Questions[1].Answer != "Psychology" {
		t.Fatalf("category 3 answer = %q", samples[0].Questions[1].Answer)
	}
	if !strings.Contains(samples[0].Turns[0].Content, "[image: a photo of a group]") {
		t.Fatalf("caption dropped: %q", samples[0].Turns[0].Content)
	}
	if !samples[0].Present.After(samples[0].Turns[0].When) {
		t.Fatal("present should be after the session")
	}

	lme := `[{
		"question_id": "q1_abs",
		"question_type": "single-session-user",
		"question": "What is my hamster's name?",
		"question_date": "2023/05/30 (Tue) 23:40",
		"answer": "not mentioned",
		"answer_session_ids": ["s1"],
		"haystack_dates": ["2023/05/20 (Sat) 02:21"],
		"haystack_session_ids": ["s1"],
		"haystack_sessions": [[{"role": "user", "content": "I have a cat."}, {"role": "assistant", "content": "Noted."}]]
	}]`
	if err := os.WriteFile(filepath.Join(dir, "longmemeval_s_cleaned.json"), []byte(lme), 0o644); err != nil {
		t.Fatal(err)
	}
	qs, err := LoadLongMemEval(dir, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || !qs[0].Abstain || len(qs[0].Turns) != 1 || qs[0].Turns[0].Session != "s1" {
		t.Fatalf("lme = %+v", qs)
	}
	if !strings.Contains(qs[0].Turns[0].Content, "User: I have a cat.") || !strings.Contains(qs[0].Turns[0].Content, "Assistant: Noted.") {
		t.Fatalf("pair = %q", qs[0].Turns[0].Content)
	}
}

func TestLoCoMoCache(t *testing.T) {
	dir, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "locomo10.json")); err != nil {
		t.Skip("locomo cache not present")
	}
	samples, err := LoadLoCoMo(dir)
	if err != nil {
		t.Fatal(err)
	}
	var nq, nt int
	for _, s := range samples {
		nq += len(s.Questions)
		nt += len(s.Turns)
	}
	if len(samples) != 10 || nq != 1540 || nt != 5882 {
		t.Fatalf("samples %d questions %d turns %d", len(samples), nq, nt)
	}
}

func TestLoadLongMemEvalCache(t *testing.T) {
	if os.Getenv("KENFOLD_BENCH_PARSE") == "" {
		t.Skip("set KENFOLD_BENCH_PARSE=1 to parse the cached LongMemEval file")
	}
	dir, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	qs, err := LoadLongMemEval(dir, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 60 {
		t.Fatalf("subset = %d", len(qs))
	}
	c := map[string]int{}
	for _, q := range qs {
		c[q.Type]++
		if len(q.Turns) == 0 || q.Answer == "" && !q.Abstain {
			t.Fatalf("empty question %+v", q.Question)
		}
	}
	if c["multi-session"] != 16 || c["single-session-preference"] != 4 {
		t.Fatalf("counts = %v", c)
	}
}

func TestReportMeans(t *testing.T) {
	r := Report{When: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), Reader: "r", Judge: "j", Embed: "e",
		Sets: []SetResult{{Name: "x", Rows: []Row{{Label: "all", N: 2, F1: 1, HasF1: true, Judge: 1, Recall5: 1, Recall10: 2, RecallN: 2}}}}}
	md := r.Markdown()
	if !strings.Contains(md, "| all | 2 | 0.500 | 0.500 | 0.500 | 1.000 | 0 |") {
		t.Fatalf("markdown = %s", md)
	}
}
