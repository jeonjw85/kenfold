package consolidate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/store"
)

// Worker makes consolidation proposals in the background.
type Worker struct {
	Store  *store.Store
	Chat   Chat
	Logger *slog.Logger

	Settle    time.Duration // memories younger than this are not judged yet (default 1h)
	MaxPairs  int           // pairs judged per run (default 4)
	DigestAge time.Duration // sessions older than this are digested (default 30 days)
	DigestMin int           // old sessions a scope needs for a digest (default 6)
	DigestMax int           // sessions per digest (default 10)
	// Yield, if set, reports whether other work for the model (extraction)
	// is waiting; the run is skipped then.
	Yield func(ctx context.Context) bool
	Now   func() time.Time
}

func (w *Worker) defaults() {
	if w.Settle <= 0 {
		w.Settle = time.Hour
	}
	if w.MaxPairs <= 0 {
		w.MaxPairs = 4
	}
	if w.DigestAge <= 0 {
		w.DigestAge = 30 * 24 * time.Hour
	}
	if w.DigestMin <= 0 {
		w.DigestMin = 6
	}
	if w.DigestMax <= 0 {
		w.DigestMax = 10
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.Logger == nil {
		w.Logger = slog.New(slog.DiscardHandler)
	}
}

// Outcome describes one run.
type Outcome struct {
	Yielded  bool             // skipped: other model work was waiting
	Judged   int              // pairs the model judged
	Distinct int              // of those, found different (or unreadable)
	Proposed []store.Proposal // new pending proposals
	Stale    int              // pending proposals whose memories changed
}

// RunOnce judges up to MaxPairs candidate pairs and writes at most one
// digest. Model errors that may pass (timeouts, overload) end the run with
// an error and are retried next time; output that fails validation is
// recorded as dismissed, so the same memories are not sent again.
func (w *Worker) RunOnce(ctx context.Context) (Outcome, error) {
	w.defaults()
	var out Outcome
	if w.Yield != nil && w.Yield(ctx) {
		out.Yielded = true
		return out, nil
	}
	var err error
	if out.Stale, err = w.Store.MarkStaleProposals(ctx); err != nil {
		return out, err
	}
	now := w.Now()
	pairs, err := w.Store.ConsolidationPairs(ctx, store.PairParams{Before: now.Add(-w.Settle), Limit: w.MaxPairs})
	if err != nil {
		return out, err
	}
	for _, pr := range pairs {
		v, err := Judge(ctx, w.Chat, pr.A, pr.B)
		if err != nil {
			if !unusable(err) {
				return out, fmt.Errorf("judge: %w", err)
			}
			w.Logger.WarnContext(ctx, "consolidation: unusable model output; the pair is not judged again", "a", pr.A.ID, "b", pr.B.ID, "err", err)
			v = Verdict{Kind: store.KindDistinct, Reason: "model output was unusable"}
		}
		out.Judged++
		p, ok, err := w.Store.CreateProposal(ctx, store.NewProposal{
			Kind: v.Kind, Scope: pr.A.Scope, MemberIDs: []string{pr.A.ID, pr.B.ID}, Keep: v.Keep, Reason: v.Reason, Model: w.Chat.Model(),
		})
		if err != nil {
			return out, err
		}
		switch {
		case !ok:
		case p.Status == store.ProposalPending:
			out.Proposed = append(out.Proposed, p)
		default:
			out.Distinct++
		}
	}

	groups, err := w.Store.DigestGroups(ctx, store.DigestParams{Before: now.Add(-w.DigestAge), Min: w.DigestMin, Max: w.DigestMax, Limit: 1})
	if err != nil {
		return out, err
	}
	for _, g := range groups {
		ids := make([]string, len(g.Sessions))
		for i, m := range g.Sessions {
			ids[i] = m.ID
		}
		np := store.NewProposal{Kind: store.KindDigest, Scope: g.Scope, MemberIDs: ids, Model: w.Chat.Model(),
			Reason: fmt.Sprintf("%d session summaries older than %d days", len(ids), int(w.DigestAge.Hours()/24))}
		content, err := Digest(ctx, w.Chat, g.Scope, g.Sessions)
		switch {
		case err == nil:
			np.Content = content
		case unusable(err):
			w.Logger.WarnContext(ctx, "consolidation: digest failed validation; these sessions are left as they are", "scope", g.Scope, "err", err)
			np.Content, np.Reason, np.Dismissed = "(not stored: the digest failed validation)", err.Error(), true
		default:
			return out, fmt.Errorf("digest: %w", err)
		}
		p, ok, err := w.Store.CreateProposal(ctx, np)
		if err != nil {
			return out, err
		}
		if ok && p.Status == store.ProposalPending {
			out.Proposed = append(out.Proposed, p)
		}
	}
	return out, nil
}

// unusable reports whether err means the model's answer cannot be used (as
// opposed to a failure that may pass, such as a timeout).
func unusable(err error) bool {
	var oe *chat.OutputError
	return errors.Is(err, ErrInvalid) || errors.Is(err, chat.ErrTruncated) || errors.As(err, &oe)
}

// Loop runs RunOnce every interval until ctx ends.
func (w *Worker) Loop(ctx context.Context, every time.Duration) {
	w.defaults()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		out, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.Logger.WarnContext(ctx, "consolidation worker", "err", err)
		}
		if len(out.Proposed) > 0 || out.Judged > 0 {
			w.Logger.InfoContext(ctx, "consolidation", "judged", out.Judged, "proposed", len(out.Proposed), "distinct", out.Distinct, "stale", out.Stale)
		}
	}
}
