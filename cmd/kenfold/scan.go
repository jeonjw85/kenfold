package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/kenfold/kenfold/internal/secrets"
	"github.com/kenfold/kenfold/internal/store"
)

// errSecretsFound makes `kenfold scan` exit non-zero when it finds secrets it
// did not redact, so it can gate scripts.
var errSecretsFound = errors.New("secrets found")

// scan checks every stored memory, in any status (deleted rows are still in
// the database), for credentials in content and attrs. Memories written before
// the Phase 2 secret filter are the expected source. With --redact it replaces
// each secret with [REDACTED:<rule>] in place.
func (c *cli) scan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	redact := fs.Bool("redact", false, "replace detected secrets in place")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("scan takes flags only")
	}
	rt, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()

	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	header := false
	found, redacted := 0, 0
	err = rt.store.ForEach(ctx, nil, func(m store.Memory) error {
		content, cf := secrets.Redact(m.Content)
		attrs, af := redactValue(m.Attrs)
		if len(cf) == 0 && len(af) == 0 {
			return nil
		}
		found++
		if !header {
			fmt.Fprintln(w, "ID\tSTATUS\tTYPE\tFIELDS\tFOUND")
			header = true
		}
		var fields []string
		if len(cf) > 0 {
			fields = append(fields, "content")
		}
		if len(af) > 0 {
			fields = append(fields, "attrs")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", m.ID, m.Status, m.Type, strings.Join(fields, ","), strings.Join(secrets.Labels(append(cf, af...)), ", "))
		if *redact {
			if _, err := rt.store.Rewrite(ctx, m.ID, content, attrs.(map[string]any), "secret scan"); err != nil {
				return fmt.Errorf("redact %s: %w", m.ID, err)
			}
			redacted++
		}
		return nil
	})
	if ferr := w.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		return err
	}
	switch {
	case found == 0:
		fmt.Fprintln(c.errOut, "No secrets found.")
		return nil
	case *redact:
		fmt.Fprintf(c.errOut, "Redacted %d memories. Rotate the affected credentials: they were stored in plain text.\n", redacted)
		if rt.embedder != nil {
			fmt.Fprintln(c.errOut, "Redacted memories will be re-embedded by the server, or run `kenfold reindex`.")
		}
		return nil
	default:
		fmt.Fprintf(c.errOut, "%d memories contain secrets. Run `kenfold scan --redact` to remove them, then rotate the credentials.\n", found)
		return errSecretsFound
	}
}

// redactValue returns a copy of v (decoded JSON) with every string redacted,
// and the findings. Map keys are left as they are.
func redactValue(v any) (any, []secrets.Finding) {
	switch x := v.(type) {
	case string:
		s, fs := secrets.Redact(x)
		return s, fs
	case map[string]any:
		out := make(map[string]any, len(x))
		var all []secrets.Finding
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys) // deterministic finding order
		for _, k := range keys {
			r, fs := redactValue(x[k])
			out[k] = r
			all = append(all, fs...)
		}
		return out, all
	case []any:
		out := make([]any, len(x))
		var all []secrets.Finding
		for i, e := range x {
			r, fs := redactValue(e)
			out[i] = r
			all = append(all, fs...)
		}
		return out, all
	case nil:
		return map[string]any{}, nil
	default:
		return v, nil
	}
}
