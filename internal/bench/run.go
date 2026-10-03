package bench

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/extract"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/retrieve"
	"github.com/kenfold/kenfold/internal/store"
)

const (
	topK         = 10
	readerTokens = 256
	judgeTokens  = 16
)

// Deps is what a run needs. Extractor is optional; when nil the extraction
// arm is skipped.
type Deps struct {
	Store     *store.Store
	Pool      *pgxpool.Pool
	Embedder  store.Embedder
	Reranker  retrieve.Reranker
	Reader    *chat.Client
	Judge     *chat.Client
	Extractor extract.Chat
	Log       func(string, ...any)
}

// Config selects the run. Limit caps questions per dataset (0 means all of
// the selected set). LMEN is the LongMemEval subset size.
type Config struct {
	DataDir string
	LMEN    int
	Limit   int
	Extract bool
}

// Report is the measured result.
type Report struct {
	When    time.Time
	Reader  string
	Judge   string
	Embed   string
	Rerank  string
	Extract string
	Notes   []string
	Sets    []SetResult
}

// SetResult is one dataset arm.
type SetResult struct {
	Name string
	Rows []Row
}

// Row is one question type, or "all".
type Row struct {
	Label    string
	N        int
	F1       float64
	HasF1    bool
	Judge    int
	Recall5  float64
	Recall10 float64
	RecallN  int
	Errors   int
}

// Run loads the datasets, stores them, and scores retrieval plus answers.
// It truncates the memory table first.
func Run(ctx context.Context, d Deps, cfg Config) (Report, error) {
	if cfg.LMEN <= 0 {
		cfg.LMEN = DefaultLMEN
	}
	log := d.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	rep := Report{
		When:   time.Now().UTC(),
		Reader: d.Reader.Model(),
		Judge:  d.Judge.Model(),
		Embed:  d.Embedder.Model(),
		Notes: []string{
			"LoCoMo category 5 (adversarial) is excluded.",
			"LoCoMo F1 is token overlap after the paper's normalization, without Porter stemming, so it is not the published F1. The judge score is the comparable number.",
			"The LongMemEval judge prompts are the official ones. A response counts as correct when it contains \"yes\".",
			"Recency uses the question date as now (LongMemEval), or one hour after the last session (LoCoMo, which has no question date).",
			"Each turn (LoCoMo) or user/assistant pair (LongMemEval) is one episodic memory, clipped to 2000 bytes so it fits the dedupe index. Retrieval is the production pipeline, top 10.",
		},
	}
	if d.Reranker != nil {
		rep.Rerank = d.Reranker.Model()
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE memory CASCADE`); err != nil {
		return rep, err
	}
	if _, err := d.Embedder.Embed(ctx, []string{"ping"}); err != nil {
		return rep, fmt.Errorf("embedder: %w", err)
	}
	if _, err := completeText(ctx, d.Reader, "", "Reply with the single word ok.", 8); err != nil {
		return rep, fmt.Errorf("reader: %w", err)
	}
	if d.Judge != d.Reader {
		if _, err := completeText(ctx, d.Judge, "", "Reply with the single word ok.", 8); err != nil {
			return rep, fmt.Errorf("judge: %w", err)
		}
	}

	samples, err := LoadLoCoMo(cfg.DataDir)
	if err != nil {
		return rep, err
	}
	if cfg.Extract && d.Extractor != nil {
		rep.Extract = d.Extractor.Model()
		rep.Notes = append(rep.Notes, "The extraction arm uses the production extractor, which was written for coding-session summaries and is not retuned for LoCoMo. It keeps at most 8 memories per session. User-wide proposals stay in the sample scope so samples do not mix, and are stored active, as if approved. Evidence recall there is by session, not by turn.")
		log("extracting LoCoMo sessions")
		if err := extractLoCoMo(ctx, d, samples, log); err != nil {
			return rep, err
		}
	}
	log("loading LongMemEval subset (%d)", cfg.LMEN)
	lme, err := LoadLongMemEval(cfg.DataDir, cfg.LMEN)
	if err != nil {
		return rep, err
	}
	if cfg.Limit > 0 && len(lme) > cfg.Limit {
		lme = lme[:cfg.Limit]
	}

	locomo, err := scoreLoCoMo(ctx, d, samples, cfg.Limit, false, log)
	if err != nil {
		return rep, err
	}
	rep.Sets = append(rep.Sets, locomo)
	if cfg.Extract && d.Extractor != nil {
		extracted, err := scoreLoCoMo(ctx, d, samples, cfg.Limit, true, log)
		if err != nil {
			return rep, err
		}
		rep.Sets = append(rep.Sets, extracted)
	}
	lmeSet, err := scoreLME(ctx, d, lme, log)
	if err != nil {
		return rep, err
	}
	rep.Sets = append(rep.Sets, lmeSet)
	return rep, nil
}

func extractLoCoMo(ctx context.Context, d Deps, samples []LoCoMoSample, log func(string, ...any)) error {
	for _, s := range samples {
		scope, err := memory.Scope("bench-locomo-x-" + s.ID)
		if err != nil {
			return err
		}
		bySession := map[string][]Turn{}
		var order []string
		for _, t := range s.Turns {
			sid := sessionOfDia(t.DiaID)
			if _, ok := bySession[sid]; !ok {
				order = append(order, sid)
			}
			bySession[sid] = append(bySession[sid], t)
		}
		var drafts []draft
		for _, sid := range order {
			turns := bySession[sid]
			var b strings.Builder
			for _, t := range turns {
				b.WriteString(t.Content)
				b.WriteByte('\n')
			}
			res, err := extract.Extract(ctx, d.Extractor, extract.Source{Project: s.ID, Content: b.String()}, extract.Options{})
			if err != nil {
				return fmt.Errorf("extract %s session %s: %w", s.ID, sid, err)
			}
			when := turns[0].When
			for _, c := range res.Candidates {
				drafts = append(drafts, draft{
					Content: c.Content, When: when, Type: c.Type, Confidence: c.Confidence,
					Session: sid,
				})
			}
			log("extracted %s session %s: %d memories", s.ID, sid, len(res.Candidates))
		}
		if err := ingest(ctx, d, scope, drafts); err != nil {
			return err
		}
	}
	return nil
}

func sessionOfDia(dia string) string {
	if i := strings.IndexByte(dia, ':'); i > 0 {
		return dia[:i]
	}
	return dia
}

func scoreLoCoMo(ctx context.Context, d Deps, samples []LoCoMoSample, limit int, extracted bool, log func(string, ...any)) (SetResult, error) {
	name := "LoCoMo categories 1-4, raw turns"
	prefix := "bench-locomo-"
	if extracted {
		name = "LoCoMo categories 1-4, extracted memories"
		prefix = "bench-locomo-x-"
	}
	var qs []scoredQ
	n := 0
samples:
	for _, s := range samples {
		if !extracted {
			scope, err := memory.Scope(prefix + s.ID)
			if err != nil {
				return SetResult{}, err
			}
			drafts := make([]draft, len(s.Turns))
			for i, t := range s.Turns {
				drafts[i] = draft{Content: t.Content, When: t.When, DiaID: t.DiaID, Type: memory.TypeEpisodic, Confidence: 0.5}
			}
			if err := ingest(ctx, d, scope, drafts); err != nil {
				return SetResult{}, err
			}
			log("ingested %s (%d turns)", s.ID, len(s.Turns))
		}
		scope, err := memory.Scope(prefix + s.ID)
		if err != nil {
			return SetResult{}, err
		}
		for _, q := range s.Questions {
			if limit > 0 && n >= limit {
				break samples
			}
			n++
			q.When = s.Present
			qs = append(qs, scoredQ{Question: q, Scope: scope})
		}
	}
	rows, err := scoreQuestions(ctx, d, qs, true, log)
	if err != nil {
		return SetResult{}, err
	}
	return SetResult{Name: name, Rows: rows}, nil
}

func scoreLME(ctx context.Context, d Deps, qs []LMEQuestion, log func(string, ...any)) (SetResult, error) {
	var scored []scoredQ
	for i, q := range qs {
		scope, err := memory.Scope("bench-lme-" + q.ID)
		if err != nil {
			return SetResult{}, err
		}
		drafts := make([]draft, len(q.Turns))
		for j, t := range q.Turns {
			drafts[j] = draft{Content: t.Content, When: t.When, Session: t.Session, Type: memory.TypeEpisodic, Confidence: 0.5}
		}
		if err := ingest(ctx, d, scope, drafts); err != nil {
			return SetResult{}, fmt.Errorf("ingest %s: %w", q.ID, err)
		}
		log("ingested LongMemEval %s (%d/%d, %d pairs)", q.ID, i+1, len(qs), len(q.Turns))
		scored = append(scored, scoredQ{Question: q.Question, Scope: scope})
	}
	rows, err := scoreQuestions(ctx, d, scored, false, log)
	if err != nil {
		return SetResult{}, err
	}
	return SetResult{Name: fmt.Sprintf("LongMemEval_S subset (%d, stratified by type)", len(qs)), Rows: rows}, nil
}

type scoredQ struct {
	Question
	Scope string
}

type draft struct {
	Content    string
	When       time.Time
	DiaID      string
	Session    string
	Type       memory.Type
	Confidence float64
}

func ingest(ctx context.Context, d Deps, scope string, drafts []draft) error {
	if len(drafts) == 0 {
		return nil
	}
	texts := make([]string, len(drafts))
	for i, dr := range drafts {
		drafts[i].Content = clipBytes(dr.Content, maxContentBytes)
		texts[i] = drafts[i].Content
	}
	vecs, err := d.Embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}
	for i, dr := range drafts {
		attrs := map[string]any{}
		if dr.DiaID != "" {
			attrs["bench_dia"] = dr.DiaID
		}
		if dr.Session != "" {
			attrs["bench_session"] = dr.Session
		}
		typ := dr.Type
		if typ == "" {
			typ = memory.TypeEpisodic
		}
		row, err := d.Store.Create(ctx, store.CreateParams{
			Type: typ, Scope: scope, Content: dr.Content, Attrs: attrs,
			SourceAgent: "bench", Trust: memory.TrustAgent, Confidence: dr.Confidence,
			Status: memory.StatusActive, Embedding: vecs[i], EmbeddingModel: d.Embedder.Model(),
		})
		if err != nil {
			return fmt.Errorf("create: %w", err)
		}
		if !dr.When.IsZero() {
			if _, err := d.Pool.Exec(ctx, `UPDATE memory SET created_at = $2, updated_at = $2 WHERE id = $1`, row.ID, dr.When); err != nil {
				return err
			}
		}
	}
	return nil
}

func scoreQuestions(ctx context.Context, d Deps, qs []scoredQ, withF1 bool, log func(string, ...any)) ([]Row, error) {
	type acc struct {
		n, judge, errors, recallN int
		f1, r5, r10               float64
	}
	by := map[string]*acc{}
	var order []string
	add := func(label string) *acc {
		if a, ok := by[label]; ok {
			return a
		}
		by[label] = &acc{}
		order = append(order, label)
		return by[label]
	}
	ret := &retrieve.Retriever{
		Store: d.Store, Embedder: d.Embedder, Reranker: d.Reranker,
		Options: retrieve.Options{RerankTimeout: 30 * time.Second},
	}
	var now time.Time
	ret.Options.Now = func() time.Time { return now }
	for i, q := range qs {
		now = q.When
		if now.IsZero() {
			now = time.Now()
		}
		hits, err := ret.Search(ctx, retrieve.Query{Text: q.Text, Scope: q.Scope, Limit: topK})
		if err != nil {
			return nil, fmt.Errorf("search %s: %w", q.ID, err)
		}
		contents := make([]string, len(hits))
		var ids5, ids10 []string
		for j, h := range hits {
			contents[j] = h.Content
			id := attrString(h.Memory, "bench_dia")
			if id == "" {
				id = attrString(h.Memory, "bench_session")
			}
			ids10 = append(ids10, id)
			if j < 5 {
				ids5 = append(ids5, id)
			}
		}
		answer, err := completeText(ctx, d.Reader, readerSystem, readerUser(q.Text, contents), readerTokens)
		if err != nil {
			add(q.Type).errors++
			add("all").errors++
			log("reader %s: %v", q.ID, err)
			continue
		}
		verdict, err := completeText(ctx, d.Judge, "", JudgePrompt(q.Question, answer), judgeTokens)
		if err != nil {
			add(q.Type).errors++
			add("all").errors++
			log("judge %s: %v", q.ID, err)
			continue
		}
		ok := JudgeYes(verdict)
		for _, label := range []string{q.Type, "all"} {
			a := add(label)
			a.n++
			if ok {
				a.judge++
			}
			if withF1 {
				a.f1 += TokenF1(answer, q.Answer, q.Type)
			}
			if r := evidenceRecall(q.Evidence, ids5); r >= 0 {
				a.r5 += r
				a.r10 += evidenceRecall(q.Evidence, ids10)
				a.recallN++
			}
		}
		if (i+1)%25 == 0 || i+1 == len(qs) {
			log("scored %d/%d", i+1, len(qs))
		}
	}
	rest := without(order, "all")
	slices.SortFunc(rest, func(a, b string) int { return cmp.Compare(labelRank(a), labelRank(b)) })
	all := rest
	if _, ok := by["all"]; ok {
		all = append([]string{"all"}, rest...)
	}
	rows := make([]Row, 0, len(all))
	for _, label := range all {
		a := by[label]
		show := label
		if label != "all" && withF1 {
			show = LoCoMoCategory(label)
		}
		rows = append(rows, Row{
			Label: show, N: a.n, F1: a.f1, HasF1: withF1, Judge: a.judge,
			Recall5: a.r5, Recall10: a.r10, RecallN: a.recallN, Errors: a.errors,
		})
	}
	return rows, nil
}

func labelRank(s string) string {
	for i, t := range LMETypes {
		if s == t {
			return fmt.Sprintf("%02d", i)
		}
	}
	return s
}

func without(ss []string, drop string) []string {
	var out []string
	for _, s := range ss {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

func attrString(m store.Memory, key string) string {
	v, ok := m.Attrs[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if ok {
		return s
	}
	return fmt.Sprint(v)
}

func completeText(ctx context.Context, c *chat.Client, system, user string, maxTokens int) (string, error) {
	var last error
	for i := 0; i < 3; i++ {
		text, _, err := c.Text(ctx, system, user, maxTokens)
		if err == nil {
			return strings.TrimSpace(text), nil
		}
		last = err
		if !chat.Retryable(err) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(i+1) * 2 * time.Second):
		}
	}
	return "", last
}

// Markdown renders the report.
func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Public benchmarks\n\n")
	fmt.Fprintf(&b, "Run: %s. Reader `%s`, judge `%s`, embeddings `%s`", r.When.Format(time.RFC3339), r.Reader, r.Judge, r.Embed)
	if r.Rerank != "" {
		fmt.Fprintf(&b, ", reranker `%s`", r.Rerank)
	}
	if r.Extract != "" {
		fmt.Fprintf(&b, ", extractor `%s`", r.Extract)
	}
	b.WriteString(".\n\n")
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	b.WriteByte('\n')
	for _, s := range r.Sets {
		fmt.Fprintf(&b, "## %s\n\n", s.Name)
		if len(s.Rows) > 0 && s.Rows[0].HasF1 {
			b.WriteString("| | n | F1 | judge | evidence recall@5 | evidence recall@10 | errors |\n|---|---:|---:|---:|---:|---:|---:|\n")
		} else {
			b.WriteString("| | n | judge | evidence recall@5 | evidence recall@10 | errors |\n|---|---:|---:|---:|---:|---:|\n")
		}
		for _, row := range s.Rows {
			if row.HasF1 {
				fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s | %d |\n",
					row.Label, row.N, meanStr(row.F1, row.N), pct(row.Judge, row.N),
					meanStr(row.Recall5, row.RecallN), meanStr(row.Recall10, row.RecallN), row.Errors)
			} else {
				fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %d |\n",
					row.Label, row.N, pct(row.Judge, row.N),
					meanStr(row.Recall5, row.RecallN), meanStr(row.Recall10, row.RecallN), row.Errors)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
