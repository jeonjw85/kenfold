package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kenfold/kenfold/internal/config"
	"github.com/kenfold/kenfold/internal/consolidate"
	"github.com/kenfold/kenfold/internal/dashboard"
	"github.com/kenfold/kenfold/internal/store"
)

// consolidationView serves consolidation proposals to the dashboard.
type consolidationView struct{ st *store.Store }

// consolidationView returns the dashboard's view of consolidation, or nil
// when consolidation is off.
func (rt *runtime) consolidationView(cfg config.Config) dashboard.Consolidation {
	if !cfg.Consolidate {
		return nil
	}
	return consolidationView{rt.store}
}

func (v consolidationView) Pending(ctx context.Context, limit int) ([]dashboard.Proposal, error) {
	ps, err := v.st.PendingProposals(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]dashboard.Proposal, len(ps))
	for i, p := range ps {
		out[i] = dashboard.Proposal{ID: p.ID, Kind: p.Kind, Scope: p.Scope, Content: p.Content, Keep: p.Keep,
			Reason: p.Reason, Model: p.Model, Members: p.Members, CreatedAt: p.CreatedAt}
	}
	return out, nil
}

func (v consolidationView) PendingCount(ctx context.Context) (int, error) {
	return v.st.CountPendingProposals(ctx)
}

func (v consolidationView) Apply(ctx context.Context, id, agent string) (string, error) {
	p, err := v.st.ApplyProposal(ctx, id, agent)
	if err != nil {
		return "", proposalErr(err)
	}
	return appliedMessage(p), nil
}

func (v consolidationView) Reject(ctx context.Context, id, agent string) error {
	return proposalErr(v.st.RejectProposal(ctx, id, agent))
}

// proposalErr explains the expected ways applying or rejecting fails.
func proposalErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrInvalidID):
		return errors.New("that proposal does not exist")
	case errors.Is(err, store.ErrProposalDecided):
		return errors.New("that proposal was already decided")
	case errors.Is(err, store.ErrProposalStale):
		return errors.New("the memories changed since the proposal was made, so nothing was changed")
	}
	return err
}

func appliedMessage(p store.Proposal) string {
	switch p.Kind {
	case store.KindDigest:
		return fmt.Sprintf("Replaced %d session summaries with a digest; they are kept as history.", len(p.MemberIDs))
	case store.KindConflict:
		return "Retired the older memory; it is kept as history."
	default:
		return "Retired the duplicate; it is kept as history."
	}
}

// consolidateCmd runs `kenfold consolidate ...`.
func (c *cli) consolidateCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErr("consolidate: missing subcommand (status|run|list|apply|reject)")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "status", "list", "run", "apply", "reject":
	default:
		return usageErr("unknown consolidate subcommand %q (status, run, list, apply, reject)", sub)
	}
	var limit *int
	if sub == "run" {
		fs := flag.NewFlagSet("consolidate run", flag.ContinueOnError)
		limit = fs.Int("limit", 20, "maximum pairs to judge")
		pos, err := parseArgs(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 0 || *limit < 1 {
			return usageErr("consolidate run takes --limit N (N >= 1)")
		}
		if !c.cfg.Chat.Enabled() {
			return errors.New("no chat model is configured; set KENFOLD_CHAT_URL (e.g. http://127.0.0.1:11434/v1) and KENFOLD_CHAT_MODEL")
		}
	}
	switch {
	case (sub == "status" || sub == "list") && len(args) != 0:
		return usageErr("consolidate %s takes no arguments", sub)
	case (sub == "apply" || sub == "reject") && len(args) == 0:
		return usageErr("consolidate %s needs proposal ids (see: kenfold consolidate list)", sub)
	}
	rt, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()
	switch sub {
	case "status":
		n, err := rt.store.ProposalCounts(ctx)
		if err != nil {
			return err
		}
		pending, err := rt.store.CountPendingProposals(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(c.out, "proposals: %d pending, %d applied, %d rejected, %d stale; %d pairs judged distinct\n",
			pending, n[store.ProposalApplied], n[store.ProposalRejected], n[store.ProposalStale]+n[store.ProposalPending]-pending, n[store.ProposalDismissed])
		if rt.chat == nil || !c.cfg.Consolidate {
			fmt.Fprintln(c.errOut, "Consolidation is off (it needs a chat model, KENFOLD_CHAT_URL), so the server makes no proposals.")
		}
		return nil
	case "list":
		return c.listProposals(ctx, rt)
	case "run":
		w := &consolidate.Worker{Store: rt.store, Chat: rt.chat, Logger: c.logger, Settle: time.Nanosecond, MaxPairs: min(*limit, 20)}
		judged, proposed := 0, 0
		for judged < *limit {
			out, err := w.RunOnce(ctx)
			if err != nil {
				return err
			}
			judged += out.Judged
			proposed += len(out.Proposed)
			for _, p := range out.Proposed {
				fmt.Fprintf(c.out, "%s  %s in %s: %s\n", p.ID, p.Kind, p.Scope, p.Reason)
			}
			if out.Judged == 0 && len(out.Proposed) == 0 {
				break
			}
		}
		fmt.Fprintf(c.out, "Judged %d pairs; %d new proposals. Review them in the dashboard or with: kenfold consolidate list\n", judged, proposed)
		return nil
	}
	var failed int
	for _, id := range args {
		var err error
		msg := "Rejected: the memories stay as they are."
		if sub == "apply" {
			var p store.Proposal
			if p, err = rt.store.ApplyProposal(ctx, id, cliAgent); err == nil {
				msg = appliedMessage(p)
			}
		} else {
			err = rt.store.RejectProposal(ctx, id, cliAgent)
		}
		if err != nil {
			failed++
			fmt.Fprintf(c.errOut, "%s: %v\n", id, proposalErr(err))
			continue
		}
		fmt.Fprintf(c.out, "%s: %s\n", id, msg)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d proposals not %s", failed, len(args), map[string]string{"apply": "applied", "reject": "rejected"}[sub])
	}
	return nil
}

func (c *cli) listProposals(ctx context.Context, rt *runtime) error {
	ps, err := rt.store.PendingProposals(ctx, 200)
	if err != nil {
		return err
	}
	if len(ps) == 0 {
		fmt.Fprintln(c.out, "No pending proposals.")
		return nil
	}
	short := func(s string, n int) string {
		s = strings.Join(strings.Fields(s), " ")
		if utf8.RuneCountInString(s) > n {
			return string([]rune(s)[:n-1]) + "…"
		}
		return s
	}
	for _, p := range ps {
		fmt.Fprintf(c.out, "%s  %s in %s (%s, %s)\n", p.ID, p.Kind, p.Scope, p.Model, p.CreatedAt.UTC().Format(time.DateOnly))
		if p.Reason != "" {
			fmt.Fprintf(c.out, "  reason: %s\n", short(p.Reason, 200))
		}
		for _, m := range p.Members {
			role := "retire"
			if m.ID == p.Keep {
				role = "keep  "
			}
			// Full ids: UUIDv7 prefixes are timestamps, shared by memories written within a minute.
			fmt.Fprintf(c.out, "  %s %s  %s (%s, %s)\n", role, m.ID, short(m.Content, 100), m.SourceAgent, m.CreatedAt.UTC().Format(time.DateOnly))
		}
		if p.Kind == store.KindDigest {
			fmt.Fprintf(c.out, "  digest: %s\n", short(p.Content, 300))
		}
	}
	return nil
}
