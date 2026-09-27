package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kenfold/kenfold/internal/apikey"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

// cliAgent is recorded as forgotten_by when the user forgets a memory from the CLI.
const cliAgent = "kenfold-cli"

// connect checks the schema, then opens the runtime. Admin commands fail
// fast with a clear message rather than on the first query.
func (c *cli) connect(ctx context.Context) (*runtime, error) {
	if err := c.checkSchema(ctx); err != nil {
		return nil, fmt.Errorf("database (KENFOLD_DATABASE_URL): %w", err)
	}
	return newRuntime(ctx, c.cfg)
}

// ---- key ----

func (c *cli) key(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErr("key: missing subcommand (create|list|revoke)")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "create":
		pos, err := parseArgs(flag.NewFlagSet("key create", flag.ContinueOnError), args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageErr("key create takes exactly one agent name, e.g. `kenfold key create claude-code`")
		}
		if !memory.ValidAgent(pos[0]) {
			return usageErr("invalid agent name %q: use lowercase letters, digits, '.', '_' or '-', e.g. claude-code", pos[0])
		}
		rt, err := c.connect(ctx)
		if err != nil {
			return err
		}
		defer rt.Close()
		plain, k, err := rt.keys.Create(ctx, pos[0])
		if err != nil {
			return err
		}
		// Only the key goes to stdout, so it can be captured:
		//   export KENFOLD_API_KEY=$(kenfold key create codex)
		fmt.Fprintln(c.out, plain)
		fmt.Fprintf(c.errOut, "Created API key %s for agent %q. It is shown only once; store it now.\n"+
			"Clients send it as \"Authorization: Bearer <key>\". See README > Connect your agents.\n", k.Prefix, k.Agent)
		return nil

	case "list":
		fs := flag.NewFlagSet("key list", flag.ContinueOnError)
		all := fs.Bool("all", false, "include revoked keys")
		pos, err := parseArgs(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 0 {
			return usageErr("key list takes no arguments")
		}
		rt, err := c.connect(ctx)
		if err != nil {
			return err
		}
		defer rt.Close()
		keys, err := rt.keys.List(ctx, *all)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			fmt.Fprintln(c.errOut, "No API keys. Create one per agent with: kenfold key create <agent>")
			return nil
		}
		w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tPREFIX\tAGENT\tCREATED\tLAST USED\tREVOKED")
		for _, k := range keys {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", k.ID, k.Prefix, k.Agent, ts(&k.CreatedAt), ts(k.LastUsedAt), ts(k.RevokedAt))
		}
		return w.Flush()

	case "revoke":
		pos, err := parseArgs(flag.NewFlagSet("key revoke", flag.ContinueOnError), args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usageErr("key revoke takes exactly one key id or prefix")
		}
		rt, err := c.connect(ctx)
		if err != nil {
			return err
		}
		defer rt.Close()
		k, err := rt.keys.Revoke(ctx, pos[0])
		switch {
		case errors.Is(err, apikey.ErrNotFound):
			return fmt.Errorf("no API key with id or prefix %q (see `kenfold key list --all`)", pos[0])
		case err != nil:
			return err
		}
		fmt.Fprintf(c.out, "revoked %s (agent %s) at %s\n", k.Prefix, k.Agent, ts(k.RevokedAt))
		return nil
	}
	return usageErr("unknown key subcommand %q (want create|list|revoke)", sub)
}

// ---- memory ----

func (c *cli) memory(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErr("memory: missing subcommand (list|approve|forget)")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "list":
		return c.memoryList(ctx, args)
	case "approve":
		pos, err := parseArgs(flag.NewFlagSet("memory approve", flag.ContinueOnError), args)
		if err != nil {
			return err
		}
		if len(pos) != 1 || !store.ValidID(pos[0]) {
			return usageErr("memory approve takes exactly one memory id")
		}
		rt, err := c.connect(ctx)
		if err != nil {
			return err
		}
		defer rt.Close()
		m, err := rt.store.Approve(ctx, pos[0])
		switch {
		case errors.Is(err, store.ErrNotFound):
			return fmt.Errorf("memory %s not found", pos[0])
		case errors.Is(err, store.ErrNotProposed):
			return fmt.Errorf("memory %s cannot be approved: %w", pos[0], err)
		case err != nil:
			return err
		}
		fmt.Fprintf(c.out, "approved %s (%s, %s): %s\n", m.ID, m.Type, m.Scope, oneLine(m.Content, 100))
		return nil
	case "forget":
		fs := flag.NewFlagSet("memory forget", flag.ContinueOnError)
		reason := fs.String("reason", "", "why the memory is wrong or no longer needed")
		pos, err := parseArgs(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 || !store.ValidID(pos[0]) {
			return usageErr("memory forget takes exactly one memory id")
		}
		rt, err := c.connect(ctx)
		if err != nil {
			return err
		}
		defer rt.Close()
		m, err := rt.store.SoftDelete(ctx, pos[0], strings.TrimSpace(*reason), cliAgent)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("memory %s not found", pos[0])
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(c.out, "forgot %s (%s): %s\n", m.ID, m.Type, oneLine(m.Content, 100))
		return nil
	}
	return usageErr("unknown memory subcommand %q (want list|approve|forget)", sub)
}

func (c *cli) memoryList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("memory list", flag.ContinueOnError)
	statuses := fs.String("status", "active", "comma-separated statuses: proposed, active, superseded, deleted")
	types := fs.String("type", "", "comma-separated memory types (default: all)")
	scope := fs.String("scope", "", "'user', 'project:<id>', or a git remote URL / project name (default: all scopes)")
	limit := fs.Int("limit", 50, "maximum rows (1-500)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("memory list takes flags only")
	}
	if *limit < 1 || *limit > 500 {
		return usageErr("--limit must be between 1 and 500")
	}
	p := store.ListParams{Limit: *limit}
	for _, s := range splitList(*statuses) {
		st := memory.Status(s)
		switch st {
		case memory.StatusProposed, memory.StatusActive, memory.StatusSuperseded, memory.StatusDeleted:
			p.Statuses = append(p.Statuses, st)
		default:
			return usageErr("unknown status %q", s)
		}
	}
	for _, s := range splitList(*types) {
		t, err := memory.ParseType(s)
		if err != nil {
			return usageErr("%v", err)
		}
		p.Types = append(p.Types, t)
	}
	if v := strings.TrimSpace(*scope); v != "" {
		if v == memory.ScopeUser || strings.HasPrefix(v, "project:") || strings.HasPrefix(v, "repo:") {
			p.Scopes = []string{v}
		} else {
			s, err := memory.Scope(v)
			if err != nil {
				return usageErr("--scope: %v", err)
			}
			p.Scopes = []string{s}
		}
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
		fmt.Fprintln(c.errOut, "No memories match.")
		return nil
	}
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTYPE\tSTATUS\tSCOPE\tAGENT\tCREATED\tCONTENT")
	for _, m := range ms {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.Type, m.Status, m.Scope, m.SourceAgent, ts(&m.CreatedAt), oneLine(m.Content, 80))
	}
	return w.Flush()
}

// ---- reindex ----

func (c *cli) reindex(ctx context.Context) error {
	if !c.cfg.Embed.Enabled() {
		return errors.New("embeddings are not configured; set KENFOLD_EMBED_URL (e.g. http://127.0.0.1:11434/v1 for Ollama)")
	}
	rt, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	err = rt.embedder.Probe(probeCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("embedding provider %s (model %s): %w", rt.embedder.Endpoint(), rt.embedder.Model(), err)
	}
	stats, err := rt.store.Backfill(ctx, rt.embedder, backfillBatch)
	if err != nil {
		return fmt.Errorf("reindex stopped after %d memories: %w", stats.Embedded, err)
	}
	fmt.Fprintf(c.out, "embedded %d memories with %s", stats.Embedded, rt.embedder.Model())
	if stats.Skipped > 0 {
		fmt.Fprintf(c.out, " (%d skipped: the provider rejected them; see logs)", stats.Skipped)
	}
	fmt.Fprintln(c.out)
	return nil
}

// ---- formatting ----

// ts formats t in local time with its zone: inside the container local time
// is UTC, on the host it is the user's zone, so the zone keeps both unambiguous.
func ts(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04 MST")
}

// oneLine collapses whitespace and control characters (so memory content
// cannot garble the terminal) and truncates to n runes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRightFunc(string(r[:n-1]), unicode.IsSpace) + "…"
}

func splitList(s string) []string {
	var out []string
	for v := range strings.SplitSeq(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
