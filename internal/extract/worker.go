package extract

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

// Agent is the source_agent of extracted memories.
const Agent = "kenfold-extractor"

// Storage policies.
const (
	// PolicyPropose stores every extracted memory as proposed; nothing is
	// served until the user approves it (default).
	PolicyPropose = "propose"
	// PolicyAuto activates confident extractions that do not resemble an
	// existing memory. Preferences are always proposed (ADR-0001).
	PolicyAuto = "auto"
)

// Worker extracts memories from session summaries in the background.
type Worker struct {
	Store    *store.Store
	Chat     Chat
	Embedder store.Embedder // optional
	Logger   *slog.Logger

	Policy         string        // PolicyPropose (default) or PolicyAuto
	AutoConfidence float64       // PolicyAuto: minimum confidence to activate (default 0.8)
	Options        Options       // extraction options
	Settle         time.Duration // summaries younger than this are not processed yet (default 2m)
	Lease          time.Duration // claim duration (default 10m)
	MaxAttempts    int           // attempts per summary for transient errors (default 5)
	Now            func() time.Time
}

func (w *Worker) defaults() {
	if w.Policy == "" {
		w.Policy = PolicyPropose
	}
	if w.AutoConfidence <= 0 {
		w.AutoConfidence = 0.8
	}
	if w.Settle <= 0 {
		w.Settle = 2 * time.Minute
	}
	if w.Lease <= 0 {
		w.Lease = 10 * time.Minute
	}
	if w.MaxAttempts <= 0 {
		w.MaxAttempts = 5
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.Logger == nil {
		w.Logger = slog.New(slog.DiscardHandler)
	}
}

// Outcome describes one processed summary.
type Outcome struct {
	SourceID   string
	Candidates int
	Stored     []store.Memory
	Skipped    int // already known (same content in the scope, in any status)
	Rejected   int // failed validation
	Err        error
}

// RunOnce processes the oldest pending summary. It returns ok=false when there
// is nothing to do. Extraction failures are recorded on the job and reported in
// Outcome.Err; the returned error is for storage problems only.
func (w *Worker) RunOnce(ctx context.Context) (Outcome, bool, error) {
	w.defaults()
	job, err := w.Store.ClaimExtraction(ctx, w.Now().Add(-w.Settle), w.Lease, w.Chat.Model())
	if errors.Is(err, store.ErrNotFound) {
		return Outcome{}, false, nil
	}
	if err != nil {
		return Outcome{}, false, fmt.Errorf("claim: %w", err)
	}
	src := job.Source
	out := Outcome{SourceID: src.ID}
	project := strings.TrimPrefix(src.Scope, "project:")
	if !strings.HasPrefix(src.Scope, "project:") {
		project = ""
	}

	res, err := Extract(ctx, w.Chat, Source{Project: project, Content: src.Content}, w.Options)
	if err != nil {
		out.Err = err
		return out, true, w.fail(ctx, job, err)
	}
	out.Candidates, out.Rejected = len(res.Candidates), len(res.Rejected)
	for _, r := range res.Rejected {
		w.Logger.DebugContext(ctx, "extraction candidate rejected", "source", src.ID, "reason", r.Reason)
	}

	for _, c := range res.Candidates {
		m, skipped, err := w.store(ctx, src, c)
		if err != nil {
			// Leave the job to be retried; memories stored so far are skipped
			// next time as already known.
			out.Err = err
			return out, true, w.fail(ctx, job, err)
		}
		if skipped {
			out.Skipped++
			continue
		}
		out.Stored = append(out.Stored, m)
	}
	if err := w.Store.FinishExtraction(ctx, src.ID, len(res.Candidates), len(out.Stored)); err != nil {
		return out, true, fmt.Errorf("finish: %w", err)
	}
	w.Logger.InfoContext(ctx, "extracted memories from a session summary",
		"source", src.ID, "candidates", len(res.Candidates), "stored", len(out.Stored), "skipped", out.Skipped, "rejected", out.Rejected,
		"prompt_tokens", res.Usage.PromptTokens, "completion_tokens", res.Usage.CompletionTokens)
	return out, true, nil
}

// store writes one candidate unless the same content already exists in its
// scope in any status (so a memory the user rejected is not proposed again).
func (w *Worker) store(ctx context.Context, src store.Memory, c Candidate) (store.Memory, bool, error) {
	scope := src.Scope
	if c.UserScope {
		scope = memory.ScopeUser
	}
	if _, err := w.Store.SeenContent(ctx, scope, c.Content); err == nil {
		return store.Memory{}, true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Memory{}, false, err
	}

	var vec []float32
	var model string
	if w.Embedder != nil {
		ectx, cancel := context.WithTimeout(ctx, 15*time.Second)
		vecs, err := w.Embedder.Embed(ectx, []string{c.Content})
		cancel()
		if err == nil && len(vecs) == 1 {
			vec, model = vecs[0], w.Embedder.Model()
		} // otherwise the backfill embeds it later
	}
	// Similar memories are never merged or skipped: a close match may be a
	// paraphrase or a contradiction (they are equally close in embedding
	// space). They are recorded for the reviewer, and block auto-activation.
	similar, err := w.Store.SimilarAny(ctx, store.SimilarParams{Scope: scope, Content: c.Content, Vector: vec, Model: model})
	if err != nil {
		return store.Memory{}, false, err
	}
	var similarIDs []string
	for _, s := range similar {
		similarIDs = append(similarIDs, s.ID)
	}

	status := memory.StatusProposed
	if w.Policy == PolicyAuto && c.Confidence >= w.AutoConfidence && len(similarIDs) == 0 && c.Type != memory.TypePreference {
		status = memory.StatusActive
	}
	attrs := map[string]any{
		"extracted_from":  src.ID,
		"evidence":        c.Evidence,
		"extractor_model": w.Chat.Model(),
		"session_agent":   src.SourceAgent,
	}
	if len(similarIDs) > 0 {
		attrs["similar_to"] = similarIDs
	}
	session := ""
	if src.SourceSession != nil {
		session = *src.SourceSession
	}
	m, err := w.Store.Create(ctx, store.CreateParams{
		Type: c.Type, Scope: scope, Content: c.Content, Attrs: attrs,
		SourceAgent: Agent, SourceSession: session, Trust: memory.TrustAgent,
		Confidence: c.Confidence, Status: status, Embedding: vec, EmbeddingModel: model,
	})
	if err != nil {
		return store.Memory{}, false, err
	}
	if err := w.Store.AddEdge(ctx, m.ID, "derived_from", src.ID, Agent); err != nil {
		return store.Memory{}, false, err
	}
	return m, false, nil
}

// fail records a failed attempt with exponential backoff for transient errors.
func (w *Worker) fail(ctx context.Context, job store.ExtractionJob, cause error) error {
	var retryAt *time.Time
	if (chat.Retryable(cause) || ctx.Err() != nil) && job.Attempts < w.MaxAttempts {
		backoff := time.Minute << (job.Attempts - 1)
		if backoff > time.Hour {
			backoff = time.Hour
		}
		t := w.Now().Add(backoff)
		retryAt = &t
	}
	if ctx.Err() == nil {
		w.Logger.WarnContext(ctx, "extraction failed", "source", job.Source.ID, "attempt", job.Attempts, "retry", retryAt != nil, "err", cause)
	}
	// Record the failure even if ctx was cancelled (server shutdown).
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	msg := cause.Error()
	if err := w.Store.FailExtraction(fctx, job.Source.ID, msg, retryAt); err != nil {
		return fmt.Errorf("record failure: %w", err)
	}
	return nil
}

// Loop drains pending summaries, then waits every interval, until ctx is done.
func (w *Worker) Loop(ctx context.Context, every time.Duration) {
	w.defaults()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for ctx.Err() == nil {
			_, ok, err := w.RunOnce(ctx)
			if err != nil && ctx.Err() == nil {
				w.Logger.WarnContext(ctx, "extraction worker", "err", err)
				break
			}
			if !ok {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
