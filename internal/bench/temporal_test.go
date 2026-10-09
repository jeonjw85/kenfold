package bench

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
)

// Omitting calendar assistance or anchoring it to Question.When instead of the
// memory would fail these reader-visible, independently dated examples.
func TestTemporalReaderCalendarHints(t *testing.T) {
	for _, tc := range []struct {
		name, date, text, want string
	}{
		{"yesterday", "8:55 pm on 25 February, 2023", "I met Jean yesterday.", `"yesterday": 2023-02-24`},
		{"tomorrow-year", "10:04 am on 31 December, 2023", "We open tomorrow.", `"tomorrow": 2024-01-01`},
		{"leap-year", "9:03 pm on 1 March, 2024", "Yesterday was special.", `"Yesterday": 2024-02-29`},
		{"days-ago", "12:10 am on 11 August, 2023", "I arrived two days ago.", `"two days ago": 2023-08-09`},
		{"weeks-ago", "12:10 am on 11 August, 2023", "I adopted Coco two weeks ago.", `"two weeks ago": approximately 2023-07-28`},
		{"last-Friday", "8:28 pm on 11 December, 2023", "A career-high last Friday.", `"last Friday": 2023-12-08`},
		{"same-weekday", "8:28 pm on 8 December, 2023", "It happened last Friday.", `"last Friday": 2023-12-01`},
		{"last-month", "9:13 pm on 9 November, 2023", "I lost my job last month.", `"last month": 2023-10-01..2023-10-31`},
		{"next-month-year", "9:13 pm on 9 December, 2023", "Next month we travel.", `"Next month": 2024-01-01..2024-01-31`},
		{"last-year", "9:32 pm on 20 April, 2022", "I visited Italy last year.", `"last year": 2021-01-01..2021-12-31`},
		{"last-week", "1:10 pm on 27 March, 2023", "I started last week.", `"last week": 2023-03-20..2023-03-26 (calendar week, Monday-Sunday; exact day unspecified)`},
		{"next-week", "1:10 pm on 29 March, 2023", "We start next week.", `"next week": 2023-04-03..2023-04-09 (calendar week, Monday-Sunday; exact day unspecified)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := "[" + tc.date + ", session 7, D7:5] Maria: " + tc.text
			q := Question{Text: "When?", Type: "2", Answer: "GOLD_SENTINEL", Evidence: []string{"PRIVATE_EVIDENCE"}, When: time.Date(2077, 1, 1, 0, 0, 0, 0, time.UTC)}
			s, u, err := readerMessages(q, []string{memory}, true, "temporal-v1")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(u, "1. "+memory+"\n") || !strings.Contains(u, "Memory 1 (D7:5)") || !strings.Contains(u, tc.want) {
				t.Fatalf("source/hint not tied to its visible memory: %s", u)
			}
			if strings.Contains(s+u, "SENTINEL") || strings.Contains(s+u, "PRIVATE_EVIDENCE") || strings.Contains(u, "2077") {
				t.Fatal("labels or synthetic question date leaked into calendar hints")
			}
			if !strings.Contains(s, "not proof") || !strings.Contains(s, "Do not invent personal facts") {
				t.Fatal("calendar assistance became an assertion of personal events")
			}
		})
	}
}

func TestTemporalReaderOnlyUsesVisibleDatedUtterances(t *testing.T) {
	for _, memory := range []string{
		"Maria: It happened yesterday.",
		"[invalid date, session 7, D7:5] Maria: It happened yesterday.",
		"[8:55 pm on 25 February, 2023, session 6, D7:5] Maria: It happened yesterday.",
		"[8:55 pm on 25 February, 2023, session 7, D7:5] Maria: Hello. [image: a poster says yesterday]",
		"[8:55 pm on 25 February, 2023, session 7, D7:5] Maria: " + strings.Repeat("a", 2100) + " yesterday.",
		"[8:55 pm on 25 February, 2023, session 7, D7:5] Maria: It happens next Friday or last summer.",
		"[8:55 pm on 25 February, 2023, session 7, D7:5] Maria: It happened 99999 days ago.",
	} {
		_, u, err := readerMessages(Question{Text: "When?", Type: "2"}, []string{memory}, true, "temporal-v1")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(u, "Calendar hints:") {
			t.Fatalf("invisible/ambiguous/unanchored input acquired a hint: %s", u)
		}
	}
}

func TestTemporalReaderDoesNotChangeLegacyOrOtherDatasets(t *testing.T) {
	memory := "[8:55 pm on 25 February, 2023, session 7, D7:5] Maria: Yesterday was fun."
	q := Question{Text: "When?", Type: "2"}
	for _, policy := range []string{"", "grounded-v2"} {
		_, user, err := readerMessages(q, []string{memory}, true, policy)
		if err != nil || user != "Memories:\n1. "+memory+"\n\nQuestion: When?" {
			t.Fatal("existing reader payload changed")
		}
	}
	q.When = time.Date(2023, 3, 1, 0, 0, 0, 0, time.UTC)
	_, user, err := readerMessages(q, []string{memory}, false, "temporal-v1")
	if err != nil || strings.Contains(user, "Calendar hints:") || !strings.HasPrefix(user, "Question date: 2023-03-01T00:00:00Z") {
		t.Fatal("LoCoMo hints altered LME handling")
	}
}

func TestTemporalReaderPolicyRequiresCapturedPrivateCache(t *testing.T) {
	t.Setenv("KENFOLD_BENCH_READER_POLICY", "temporal-v1")
	t.Setenv("KENFOLD_BENCH_CAPTURE_INPUTS", "1")
	t.Setenv("KENFOLD_BENCH_CONTEXT_POLICY", "")
	options, err := answerOptionsFromEnv()
	if err != nil || options.ReaderPolicy != "temporal-v1" || !options.CaptureInputs {
		t.Fatalf("new policy cannot be explicitly selected: %+v %v", options, err)
	}
	if err := validateAnswerOptions(options, nil); err == nil {
		t.Fatal("new policy accepted without private captured-input cache")
	}
	cache, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "temporal-policy-fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err := validateAnswerOptions(options, cache); err != nil {
		t.Fatal(err)
	}
	options.CaptureInputs = false
	if err := validateAnswerOptions(options, cache); err == nil {
		t.Fatal("new policy accepted without durable captured inputs")
	}
}

func TestTemporalReaderSnapshotsActualCalendarPayload(t *testing.T) {
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, scoreFixturePool(t), f)
	qs := scoreFixtureQuestions(t, d)[:1]
	content := "[8:55 pm on 25 February, 2023, session 7, D7:5] Maria: I met Jean yesterday."
	if _, err := d.Pool.Exec(context.Background(), `UPDATE memory SET content=$1 WHERE scope=$2`, content, qs[0].Scope); err != nil {
		t.Fatal(err)
	}
	cache, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "temporal-http-fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	d.ScoreCache = cache
	d.AnswerOptions = AnswerOptions{ReaderPolicy: "temporal-v1", CaptureInputs: true}
	f.inspectChat = func(model string, messages []chat.Message) {
		if model != "fixture-reader" {
			return
		}
		state, _ := scoreInputsWire(t, scoreQuestionRecordPath(t, cache.dir))
		inputs := state["inputs"].(map[string]any)
		if state["phase"] != "reader-in-flight" || inputs["reader_user"] != messages[1].Content || !strings.Contains(messages[1].Content, `"yesterday": 2023-02-24`) {
			t.Error("calendar input is not the exact durable pre-call HTTP payload")
		}
	}
	if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err != nil {
		t.Fatal(err)
	}
	d.AnswerOptions.ReaderPolicy = "grounded-v2"
	if _, err := scoreQuestions(context.Background(), d, qs, true, t.Logf); err == nil {
		t.Fatal("new calendar input silently reused an old policy slot")
	}
	if f.reader.Load() != 1 || f.judge.Load() != 1 {
		t.Fatal("policy drift repeated paid-boundary calls")
	}
}

func TestTemporalReaderRejectsPartialAndQualifiedExpressions(t *testing.T) {
	for _, text := range []string{
		"1.5 days ago", "1,365 days ago", "twenty two days ago", "2-3 days ago",
		"twenty-two days ago", "one hundred two days ago", "-2 days ago", "éyesterday",
		"about two days ago", "more than two days ago", "less than two days ago",
		"at least two days ago", "at most two days ago", "approximately yesterday",
		"two or three days ago", "two to three days ago", "two / three days ago",
		"two days ago or so", "yesterday or tomorrow", "around last month",
	} {
		t.Run(text, func(t *testing.T) {
			memory := "[8:55 pm on 25 February, 2023, session 7, D7:5] Maria: I arrived " + text + "."
			_, user, err := readerMessages(Question{Text: "When?"}, []string{memory}, true, "temporal-v1")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(user, "Calendar hints:") || !strings.Contains(user, memory) {
				t.Fatalf("partial/uncertain expression became an exact calendar hint: %s", user)
			}
		})
	}
	header := "[8:55 pm on 25 February, 2023, session 7, D7:5] Maria: "
	// The reader-visible clip ends after "tomorrow", but the source word is longer.
	memory := header + strings.Repeat("x", 2000-len(header)-len(" tomorrow")) + " tomorrowland"
	_, user, err := readerMessages(Question{Text: "When?"}, []string{memory}, true, "temporal-v1")
	if err != nil || strings.Contains(user, "Calendar hints:") {
		t.Fatal("clipped word ending manufactured a calendar expression")
	}
}

func TestTemporalReaderNamesWeekdayConvention(t *testing.T) {
	memory := "[8:28 pm on 8 December, 2023, session 7, D7:5] Maria: It happened last Friday."
	_, user, err := readerMessages(Question{Text: "When?"}, []string{memory}, true, "temporal-v1")
	if err != nil || !strings.Contains(user, `"last Friday": 2023-12-01 (strictly preceding weekday convention; original context decides)`) {
		t.Fatalf("a weekday convention was presented as an unqualified event date: %s", user)
	}
}
