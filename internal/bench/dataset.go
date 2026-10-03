// Package bench measures Kenfold on public memory benchmarks: LoCoMo
// (categories 1–4) and a stratified subset of LongMemEval_S. It stores
// conversations as memories, retrieves with the production pipeline, and
// scores a reader model's answers. Datasets are downloaded into a cache and
// are not committed.
package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	locomoURL = "https://raw.githubusercontent.com/snap-research/locomo/main/data/locomo10.json"
	lmeURL    = "https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_s_cleaned.json"

	locomoFile = "locomo10.json"
	lmeFile    = "longmemeval_s_cleaned.json"

	// DefaultLMEN is the LongMemEval_S subset size: stratified by question
	// type, the first questions of each type by id, so a rerun uses the same
	// questions.
	DefaultLMEN = 60
)

// LMETypes is the LongMemEval question-type order used in reports.
var LMETypes = []string{
	"single-session-user",
	"single-session-assistant",
	"single-session-preference",
	"multi-session",
	"temporal-reasoning",
	"knowledge-update",
}

// DefaultDir is the dataset cache ($HOME/.cache/kenfold/bench). It is not
// os.UserCacheDir: on macOS that is ~/Library/Caches, and the files are kept
// next to other tool caches under ~/.cache.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "kenfold", "bench"), nil
}

// Ensure downloads a missing dataset into dir. A present non-empty file is
// left as it is.
func Ensure(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []struct{ name, url string }{{locomoFile, locomoURL}, {lmeFile, lmeURL}} {
		path := filepath.Join(dir, f.name)
		st, err := os.Stat(path)
		if err == nil && st.Size() > 1000 {
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := download(path, f.url); err != nil {
			return err
		}
	}
	return nil
}

func download(path, rawURL string) error {
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(rawURL)
	if err != nil {
		return fmt.Errorf("download %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", rawURL, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, copyErr := io.Copy(tmp, resp.Body)
	closeErr := tmp.Close()
	if copyErr != nil {
		os.Remove(tmpName)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(tmpName)
		return closeErr
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Turn is one memory to store: a dialogue turn, or a user/assistant pair.
type Turn struct {
	Content string
	When    time.Time
	DiaID   string // LoCoMo evidence id, e.g. D1:3
	Session string // LongMemEval session id
}

// LoCoMoSample is one conversation and its questions (categories 1–4).
type LoCoMoSample struct {
	ID        string
	Turns     []Turn
	Questions []Question
	// Present is one hour after the last session, used as "now" for recency.
	// LoCoMo questions have no question date.
	Present time.Time
}

// Question is one scored question.
type Question struct {
	ID       string
	Type     string // "1".."4" or a LongMemEval question type
	Text     string
	Answer   string
	Evidence []string // dia ids, or session ids
	Abstain  bool
	When     time.Time // question date; zero for LoCoMo (use the sample's Present)
}

// LMEQuestion is one LongMemEval question with its own haystack.
type LMEQuestion struct {
	Question
	Turns []Turn
}

// LoadLoCoMo reads locomo10.json. Category 5 (adversarial) is omitted: the
// paper and later memory systems score it separately, and it has no answer
// string.
func LoadLoCoMo(dir string) ([]LoCoMoSample, error) {
	raw, err := os.ReadFile(filepath.Join(dir, locomoFile))
	if err != nil {
		return nil, err
	}
	var file []struct {
		SampleID     string                     `json:"sample_id"`
		QA           []locomoQA                 `json:"qa"`
		Conversation map[string]json.RawMessage `json:"conversation"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("locomo: %w", err)
	}
	out := make([]LoCoMoSample, 0, len(file))
	for _, s := range file {
		sample, err := locomoSample(s.SampleID, s.Conversation, s.QA)
		if err != nil {
			return nil, fmt.Errorf("locomo %s: %w", s.SampleID, err)
		}
		out = append(out, sample)
	}
	return out, nil
}

type locomoQA struct {
	Question string     `json:"question"`
	Answer   answerText `json:"answer"`
	Evidence []string   `json:"evidence"`
	Category int        `json:"category"`
}

type locomoTurn struct {
	Speaker     string `json:"speaker"`
	DiaID       string `json:"dia_id"`
	Text        string `json:"text"`
	BlipCaption string `json:"blip_caption"`
}

func locomoSample(id string, conv map[string]json.RawMessage, qas []locomoQA) (LoCoMoSample, error) {
	var nums []int
	for k := range conv {
		n, ok := sessionNumber(k)
		if ok {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	sample := LoCoMoSample{ID: id}
	for _, n := range nums {
		var turns []locomoTurn
		if err := json.Unmarshal(conv[fmt.Sprintf("session_%d", n)], &turns); err != nil {
			return LoCoMoSample{}, fmt.Errorf("session %d: %w", n, err)
		}
		var dateRaw string
		if raw, ok := conv[fmt.Sprintf("session_%d_date_time", n)]; ok {
			if err := json.Unmarshal(raw, &dateRaw); err != nil {
				return LoCoMoSample{}, err
			}
		}
		when, err := parseLoCoMoDate(dateRaw)
		if err != nil {
			return LoCoMoSample{}, fmt.Errorf("session %d date %q: %w", n, dateRaw, err)
		}
		if when.After(sample.Present) {
			sample.Present = when
		}
		for _, t := range turns {
			text := strings.TrimSpace(t.Text)
			if cap := strings.TrimSpace(t.BlipCaption); cap != "" {
				text += " [image: " + cap + "]"
			}
			if text == "" || t.DiaID == "" {
				continue
			}
			sample.Turns = append(sample.Turns, Turn{
				Content: fmt.Sprintf("[%s, session %d, %s] %s: %s", dateRaw, n, t.DiaID, t.Speaker, text),
				When:    when,
				DiaID:   t.DiaID,
			})
		}
	}
	if sample.Present.IsZero() {
		return LoCoMoSample{}, fmt.Errorf("no sessions")
	}
	sample.Present = sample.Present.Add(time.Hour)
	for i, q := range qas {
		if q.Category < 1 || q.Category > 4 || strings.TrimSpace(q.Answer.Text) == "" {
			continue
		}
		answer := q.Answer.Text
		if q.Category == 3 {
			answer = strings.TrimSpace(strings.Split(answer, ";")[0])
		}
		sample.Questions = append(sample.Questions, Question{
			ID:       fmt.Sprintf("%s-%d", id, i),
			Type:     strconv.Itoa(q.Category),
			Text:     q.Question,
			Answer:   answer,
			Evidence: q.Evidence,
		})
	}
	return sample, nil
}

func sessionNumber(key string) (int, bool) {
	rest, ok := strings.CutPrefix(key, "session_")
	if !ok || strings.Contains(rest, "_") {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil
}

// LoadLongMemEval reads the cleaned LongMemEval_S file and returns a
// stratified subset of n questions (or all of them when n is greater).
func LoadLongMemEval(dir string, n int) ([]LMEQuestion, error) {
	f, err := os.Open(filepath.Join(dir, lmeFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("longmemeval: want a JSON array")
	}
	var all []LMEQuestion
	for dec.More() {
		var raw lmeRaw
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("longmemeval: %w", err)
		}
		q, err := raw.question()
		if err != nil {
			return nil, fmt.Errorf("longmemeval %s: %w", raw.ID, err)
		}
		all = append(all, q)
	}
	return subsetLME(all, n), nil
}

type lmeRaw struct {
	ID           string     `json:"question_id"`
	Type         string     `json:"question_type"`
	Question     string     `json:"question"`
	QuestionDate string     `json:"question_date"`
	Answer       answerText `json:"answer"`
	AnswerIDs    []string   `json:"answer_session_ids"`
	Dates        []string   `json:"haystack_dates"`
	SessionIDs   []string   `json:"haystack_session_ids"`
	Sessions     [][]struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"haystack_sessions"`
}

func (r lmeRaw) question() (LMEQuestion, error) {
	when, err := parseLMEDate(r.QuestionDate)
	if err != nil {
		return LMEQuestion{}, fmt.Errorf("question date %q: %w", r.QuestionDate, err)
	}
	if len(r.Sessions) != len(r.SessionIDs) || len(r.Sessions) != len(r.Dates) {
		return LMEQuestion{}, fmt.Errorf("haystack length mismatch")
	}
	q := LMEQuestion{Question: Question{
		ID: r.ID, Type: r.Type, Text: r.Question, Answer: r.Answer.Text,
		Evidence: r.AnswerIDs, Abstain: strings.Contains(r.ID, "_abs"), When: when,
	}}
	for i, sess := range r.Sessions {
		at, err := parseLMEDate(r.Dates[i])
		if err != nil {
			return LMEQuestion{}, fmt.Errorf("session date %q: %w", r.Dates[i], err)
		}
		q.Turns = append(q.Turns, pairTurns(r.Dates[i], r.SessionIDs[i], at, sess)...)
	}
	return q, nil
}

func pairTurns(date, session string, when time.Time, turns []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) []Turn {
	var out []Turn
	var user string
	flush := func(assistant string) {
		var b strings.Builder
		fmt.Fprintf(&b, "[%s, session %s]\n", date, session)
		if user != "" {
			fmt.Fprintf(&b, "User: %s\n", clip(strings.TrimSpace(user), 2500))
		}
		if assistant != "" {
			fmt.Fprintf(&b, "Assistant: %s\n", clip(strings.TrimSpace(assistant), 2500))
		}
		text := strings.TrimSpace(b.String())
		if text == "" || (user == "" && assistant == "") {
			user = ""
			return
		}
		out = append(out, Turn{Content: clip(text, 5000), When: when, Session: session})
		user = ""
	}
	for _, t := range turns {
		switch t.Role {
		case "user":
			if user != "" {
				flush("")
			}
			user = t.Content
		case "assistant":
			flush(t.Content)
		default:
			if user != "" {
				flush("")
			}
			flush(t.Content)
		}
	}
	if user != "" {
		flush("")
	}
	return out
}

// subsetLME keeps n questions, spread across types by largest remainder, and
// within a type the lowest question ids. n <= 0 or n >= len returns every
// question, ordered by type then id.
func subsetLME(all []LMEQuestion, n int) []LMEQuestion {
	byType := map[string][]LMEQuestion{}
	for _, q := range all {
		byType[q.Type] = append(byType[q.Type], q)
	}
	for _, qs := range byType {
		sort.Slice(qs, func(i, j int) bool { return qs[i].ID < qs[j].ID })
	}
	if n <= 0 || n >= len(all) {
		n = len(all)
	}
	alloc := allocate(byType, n)
	var out []LMEQuestion
	for _, typ := range typeOrder(byType) {
		qs := byType[typ]
		k := alloc[typ]
		if k > len(qs) {
			k = len(qs)
		}
		out = append(out, qs[:k]...)
	}
	return out
}

func allocate(byType map[string][]LMEQuestion, n int) map[string]int {
	total := 0
	for _, qs := range byType {
		total += len(qs)
	}
	type part struct {
		k    string
		frac float64
		n    int
		left int
	}
	parts := make([]part, 0, len(byType))
	sum := 0
	for k, qs := range byType {
		exact := float64(n) * float64(len(qs)) / float64(total)
		base := int(exact)
		if base > len(qs) {
			base = len(qs)
		}
		parts = append(parts, part{k, exact - float64(base), base, len(qs) - base})
		sum += base
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].frac != parts[j].frac {
			return parts[i].frac > parts[j].frac
		}
		return parts[i].k < parts[j].k
	})
	for sum < n {
		moved := false
		for i := range parts {
			if parts[i].left == 0 {
				continue
			}
			parts[i].n++
			parts[i].left--
			sum++
			moved = true
			if sum == n {
				break
			}
		}
		if !moved {
			break
		}
	}
	out := make(map[string]int, len(parts))
	for _, p := range parts {
		out[p.k] = p.n
	}
	return out
}

func typeOrder(byType map[string][]LMEQuestion) []string {
	seen := map[string]bool{}
	var order []string
	for _, t := range LMETypes {
		if _, ok := byType[t]; ok {
			order = append(order, t)
			seen[t] = true
		}
	}
	var rest []string
	for t := range byType {
		if !seen[t] {
			rest = append(rest, t)
		}
	}
	sort.Strings(rest)
	return append(order, rest...)
}

// answerText accepts a JSON string or number. LoCoMo leaves some answers null.
type answerText struct{ Text string }

func (a *answerText) UnmarshalJSON(b []byte) error {
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		return json.Unmarshal(b, &a.Text)
	}
	a.Text = string(b)
	return nil
}
