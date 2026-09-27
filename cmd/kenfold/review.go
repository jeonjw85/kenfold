package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kenfold/kenfold/internal/extract"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

// approveMany approves proposed memories by id. With --replaces, a single
// proposed memory replaces an active one.
func (c *cli) approveMany(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("memory approve", flag.ContinueOnError)
	replaces := fs.String("replaces", "", "id of an active memory the approved one replaces (it becomes superseded)")
	ids, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return usageErr("memory approve takes one or more memory ids")
	}
	for _, id := range ids {
		if !store.ValidID(id) {
			return usageErr("%q is not a memory id", id)
		}
	}
	if *replaces != "" && (len(ids) != 1 || !store.ValidID(*replaces)) {
		return usageErr("--replaces takes one memory id and applies to exactly one approved memory")
	}
	rt, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()
	failed := 0
	for _, id := range ids {
		var m store.Memory
		if *replaces != "" {
			m, err = rt.store.ApproveReplacing(ctx, id, *replaces)
		} else {
			m, err = rt.store.Approve(ctx, id)
		}
		if err != nil {
			failed++
			fmt.Fprintf(c.errOut, "%s: %v\n", id, reviewErr(err))
			continue
		}
		fmt.Fprintf(c.out, "approved %s (%s, %s): %s\n", m.ID, m.Type, m.Scope, oneLine(m.Content, 100))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d memories were not approved", failed, len(ids))
	}
	return nil
}

// rejectMany rejects proposed memories (soft-deletes them). The extractor does
// not propose a rejected statement again.
func (c *cli) rejectMany(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("memory reject", flag.ContinueOnError)
	reason := fs.String("reason", "", "why the memory is wrong or not worth keeping")
	ids, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return usageErr("memory reject takes one or more memory ids")
	}
	for _, id := range ids {
		if !store.ValidID(id) {
			return usageErr("%q is not a memory id", id)
		}
	}
	rt, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()
	failed := 0
	for _, id := range ids {
		m, err := rt.store.Reject(ctx, id, strings.TrimSpace(*reason), cliAgent)
		if err != nil {
			failed++
			fmt.Fprintf(c.errOut, "%s: %v\n", id, reviewErr(err))
			continue
		}
		fmt.Fprintf(c.out, "rejected %s (%s): %s\n", m.ID, m.Type, oneLine(m.Content, 100))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d memories were not rejected", failed, len(ids))
	}
	return nil
}

func reviewErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errors.New("not found")
	case errors.Is(err, store.ErrNotProposed):
		return fmt.Errorf("not awaiting review: %w", err)
	}
	return err
}

// review walks through proposed memories interactively.
func (c *cli) review(ctx context.Context, args []string, in io.Reader) error {
	fs := flag.NewFlagSet("memory review", flag.ContinueOnError)
	scope := fs.String("scope", "", "only memories in this scope ('user', 'project:<id>', or a git remote URL)")
	limit := fs.Int("limit", 50, "maximum memories to review (1-500)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("memory review takes flags only")
	}
	if *limit < 1 || *limit > 500 {
		return usageErr("--limit must be between 1 and 500")
	}
	p := store.ListParams{Statuses: []memory.Status{memory.StatusProposed}, Limit: *limit}
	if v := strings.TrimSpace(*scope); v != "" {
		s, err := scopeArg(v)
		if err != nil {
			return usageErr("--scope: %v", err)
		}
		p.Scopes = []string{s}
	}
	rt, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()
	ms, err := rt.store.List(ctx, p)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		fmt.Fprintln(c.out, "Nothing to review.")
		return nil
	}
	// Oldest first: review in the order memories were proposed.
	for i, j := 0, len(ms)-1; i < j; i, j = i+1, j-1 {
		ms[i], ms[j] = ms[j], ms[i]
	}

	sc := bufio.NewScanner(in)
	var approved, rejected, skipped int
	for i, m := range ms {
		fmt.Fprintf(c.out, "\n[%d/%d] %s  %s  %s\n", i+1, len(ms), m.Type, m.Scope, m.ID)
		fmt.Fprintf(c.out, "  %s\n", oneLine(m.Content, 400))
		c.printProvenance(m)
		var similar []store.Memory
		for _, id := range attrStrings(m.Attrs, "similar_to") {
			if s, err := rt.store.Get(ctx, id); err == nil && s.Status == memory.StatusActive {
				similar = append(similar, s)
			}
		}
		for n, s := range similar {
			fmt.Fprintf(c.out, "  similar #%d (active, %s): %s\n", n+1, s.SourceAgent, oneLine(s.Content, 200))
		}
		prompt := "  [a]pprove  [r]eject  [s]kip  [q]uit"
		if len(similar) > 0 {
			prompt += "  [N] approve and replace similar #N"
		}
	ask:
		fmt.Fprint(c.out, prompt+"? ")
		if !sc.Scan() {
			fmt.Fprintln(c.out)
			break
		}
		ans := strings.ToLower(strings.TrimSpace(sc.Text()))
		var act error
		switch {
		case ans == "a":
			_, act = rt.store.Approve(ctx, m.ID)
			if act == nil {
				approved++
			}
		case ans == "r":
			_, act = rt.store.Reject(ctx, m.ID, "rejected in review", cliAgent)
			if act == nil {
				rejected++
			}
		case ans == "s" || ans == "":
			skipped++
		case ans == "q":
			fmt.Fprintf(c.out, "\nApproved %d, rejected %d, skipped %d.\n", approved, rejected, skipped)
			return nil
		case len(ans) == 1 && ans[0] >= '1' && int(ans[0]-'0') <= len(similar):
			_, act = rt.store.ApproveReplacing(ctx, m.ID, similar[ans[0]-'1'].ID)
			if act == nil {
				approved++
			}
		default:
			goto ask
		}
		if act != nil {
			fmt.Fprintf(c.out, "  %v\n", reviewErr(act))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "\nApproved %d, rejected %d, skipped %d.\n", approved, rejected, skipped)
	return nil
}

// printProvenance shows where a proposed memory came from.
func (c *cli) printProvenance(m store.Memory) {
	if m.SourceAgent != extract.Agent {
		fmt.Fprintf(c.out, "  from %s, %s\n", m.SourceAgent, ts(&m.CreatedAt))
		return
	}
	agent, _ := m.Attrs["session_agent"].(string)
	model, _ := m.Attrs["extractor_model"].(string)
	fmt.Fprintf(c.out, "  extracted by %s from a %s session, %s (confidence %.2f)\n", model, agent, ts(&m.CreatedAt), m.Confidence)
	if ev, _ := m.Attrs["evidence"].(string); ev != "" {
		fmt.Fprintf(c.out, "  evidence: “%s”\n", oneLine(ev, 200))
	}
}

func attrStrings(attrs map[string]any, key string) []string {
	raw, _ := attrs[key].([]any)
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok && store.ValidID(s) {
			out = append(out, s)
		}
	}
	return out
}

func scopeArg(v string) (string, error) {
	if v == memory.ScopeUser || strings.HasPrefix(v, "project:") || strings.HasPrefix(v, "repo:") {
		return v, nil
	}
	return memory.Scope(v)
}

// ---- extract ----

func (c *cli) extract(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErr("extract: missing subcommand (status|run)")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "status":
		if len(args) != 0 {
			return usageErr("extract status takes no arguments")
		}
		rt, err := c.connect(ctx)
		if err != nil {
			return err
		}
		defer rt.Close()
		st, err := rt.store.ExtractionStatus(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(c.out, "session summaries: %d pending, %d running, %d done, %d failed; %d memories extracted\n",
			st.Pending, st.Running, st.Done, st.Failed, st.Stored)
		if rt.chat == nil {
			fmt.Fprintln(c.errOut, "No chat model is configured (KENFOLD_CHAT_URL), so the server does not extract.")
		}
		return nil
	case "run":
		fs := flag.NewFlagSet("extract run", flag.ContinueOnError)
		limit := fs.Int("limit", 20, "maximum summaries to process")
		pos, err := parseArgs(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 0 || *limit < 1 {
			return usageErr("extract run takes --limit N (N >= 1)")
		}
		if !c.cfg.Chat.Enabled() {
			return errors.New("no chat model is configured; set KENFOLD_CHAT_URL (e.g. http://127.0.0.1:11434/v1) and KENFOLD_CHAT_MODEL")
		}
		rt, err := c.connect(ctx)
		if err != nil {
			return err
		}
		defer rt.Close()
		w := &extract.Worker{Store: rt.store, Chat: rt.chat, Embedder: rt.embedderIface(), Logger: c.logger,
			Policy: c.cfg.ExtractPolicy, Settle: time.Nanosecond}
		done, stored, failed := 0, 0, 0
		for done < *limit {
			out, ok, err := w.RunOnce(ctx)
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			done++
			if out.Err != nil {
				failed++
				fmt.Fprintf(c.errOut, "%s: %v\n", out.SourceID, out.Err)
				continue
			}
			stored += len(out.Stored)
			for _, m := range out.Stored {
				fmt.Fprintf(c.out, "%s  %-10s %-8s %s\n", m.ID, m.Type, m.Status, oneLine(m.Content, 90))
			}
		}
		fmt.Fprintf(c.errOut, "Processed %d summaries (%d failed); stored %d memories. Review them with `kenfold memory review`.\n", done, failed, stored)
		return nil
	}
	return usageErr("unknown extract subcommand %q (want status|run)", sub)
}
