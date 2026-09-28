package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/kenfold/kenfold/internal/coderef"
	"github.com/kenfold/kenfold/internal/hook"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

// refsSync runs `kenfold refs sync`: it checks the current repository's code
// references against HEAD and reports them to the server, like the session
// hook does at session start. It needs the repository, not the database, so
// it runs wherever the code is (e.g. from a git post-commit hook).
func refsSync(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("refs sync", flag.ContinueOnError)
	dir := fs.String("dir", ".", "a directory inside the repository")
	url := fs.String("url", envOr(getenv, "KENFOLD_URL", hook.DefaultURL), "Kenfold MCP endpoint (the API is next to it)")
	keyFile := fs.String("key-file", "", "file containing an API key")
	keyEnv := fs.String("key-env", "KENFOLD_API_KEY", "environment variable holding the API key")
	quiet := fs.Bool("quiet", false, "print nothing on success")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("refs sync takes flags only")
	}
	key, err := readKey(getenv, *keyFile, *keyEnv)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	project, _ := hook.DetectProject(ctx, abs)
	if project == "" {
		return fmt.Errorf("%s is not inside a git repository with a usable project identity", abs)
	}
	base, err := coderef.APIBase(*url)
	if err != nil {
		return err
	}
	res, err := coderef.Sync(ctx, coderef.SyncOptions{APIBase: base, APIKey: key, Dir: abs, Project: project})
	if errors.Is(err, coderef.ErrNoCommits) {
		if !*quiet {
			fmt.Fprintln(stdout, "The repository has no commits yet; nothing to check.")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("refs sync (%s): %w", project, err)
	}
	if *quiet {
		return nil
	}
	if res.Targets == 0 {
		fmt.Fprintf(stdout, "No memories of %s refer to code.\n", project)
		return nil
	}
	fmt.Fprintf(stdout, "Checked %d of %d references of %s at %s", res.Checked, res.Targets, project, short(res.Commit))
	if res.Checked < res.Targets {
		fmt.Fprint(stdout, " (the rest were anchored at commits this checkout does not contain, or wait for uncommitted changes)")
	}
	fmt.Fprintln(stdout, ".")
	if len(res.Summary.States) > 0 {
		fmt.Fprintf(stdout, "Now: %s.\n", formatStates(res.Summary.States))
	}
	return nil
}

// readKey returns the API key from keyFile, or else from the keyEnv variable.
func readKey(getenv func(string) string, keyFile, keyEnv string) (string, error) {
	if keyFile == "" {
		return getenv(keyEnv), nil
	}
	b, err := os.ReadFile(expandHome(keyFile, getenv))
	if err != nil {
		return "", fmt.Errorf("read key file: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// refs runs the database-side `kenfold refs` subcommands.
func (c *cli) refs(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "status" {
		return usageErr("usage: kenfold refs sync [flags] | kenfold refs status [--scope S]")
	}
	fs := flag.NewFlagSet("refs status", flag.ContinueOnError)
	scopeArg := fs.String("scope", "", "'project:<id>' or a git remote URL / project name (default: all projects)")
	limit := fs.Int("limit", 50, "maximum stale memories to list (1-500)")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("refs status takes flags only")
	}
	if *limit < 1 || *limit > 500 {
		return usageErr("--limit must be between 1 and 500")
	}
	scope := ""
	if v := strings.TrimSpace(*scopeArg); v != "" {
		if strings.HasPrefix(v, "project:") || strings.HasPrefix(v, "repo:") {
			scope = v
		} else if scope, err = memory.Scope(v); err != nil || scope == memory.ScopeUser {
			return usageErr("--scope: a project is required")
		}
	}
	rt, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()
	counts, err := rt.store.RefCounts(ctx, scope)
	if err != nil {
		return err
	}
	if len(counts) == 0 {
		fmt.Fprintln(c.out, "No memories refer to code.")
		return nil
	}
	fmt.Fprintf(c.out, "Code references: %s.\n", formatStates(counts))
	if counts[store.RefPending] > 0 {
		fmt.Fprintln(c.out, "Pending references are anchored by the session hook or `kenfold refs sync` in the repository.")
	}
	p := store.ListParams{Stale: true, Limit: *limit}
	if scope != "" {
		p.Scopes = []string{scope}
	}
	stale, err := rt.store.List(ctx, p)
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}
	ids := make([]string, len(stale))
	for i, m := range stale {
		ids[i] = m.ID
	}
	refs, err := rt.store.Refs(ctx, ids)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "\n%d memories may be outdated (review, then supersede or `kenfold memory forget`):\n", len(stale))
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTYPE\tSCOPE\tCODE\tCONTENT")
	for _, m := range stale {
		var why []string
		for _, r := range refs[m.ID] {
			if r.Stale() {
				why = append(why, describeRef(r))
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", m.ID, m.Type, m.Scope, strings.Join(why, "; "), oneLine(m.Content, 60))
	}
	return w.Flush()
}

func describeRef(r store.CodeRef) string {
	what := r.Path
	if r.Symbol != "" {
		what = r.Symbol
		if r.Path != "" {
			what += " in " + r.Path
		}
	}
	at := ""
	if r.CheckedCommit != nil {
		at = " at " + short(*r.CheckedCommit)
	}
	return what + " " + r.State + at
}

// formatStates renders state counts in lifecycle order.
func formatStates(m map[string]int) string {
	order := []string{store.RefCurrent, store.RefChanged, store.RefMissing, store.RefPending, store.RefUnresolved}
	var parts []string
	for _, st := range order {
		if n := m[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st))
		}
	}
	for st, n := range m {
		if !slices.Contains(order, st) && n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st))
		}
	}
	return strings.Join(parts, ", ")
}

func short(commit string) string { return commit[:min(12, len(commit))] }
