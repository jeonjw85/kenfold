package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kenfold/kenfold/internal/buildinfo"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/secrets"
	"github.com/kenfold/kenfold/internal/store"
)

const maxArchiveLine = 1 << 20 // one record; memories are at most 8000 characters

// export writes an archive (JSON Lines: a header, the records, and an end
// record with the counts) to --out or stdout.
func (c *cli) export(ctx context.Context, args []string) (err error) {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fs.String("out", "-", "file to write (created with mode 0600; must not exist), or - for stdout")
	scopeArg := fs.String("scope", "", "only this scope: 'user', 'project:<id>', or a git remote URL / project name")
	activeOnly := fs.Bool("active", false, "only active and proposed memories (default: everything, including history)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("export takes flags only")
	}
	var p store.ExportParams
	if v := strings.TrimSpace(*scopeArg); v != "" {
		if v == memory.ScopeUser || strings.HasPrefix(v, "project:") || strings.HasPrefix(v, "repo:") {
			p.Scopes = []string{v}
		} else if s, err := memory.Scope(v); err == nil {
			p.Scopes = []string{s}
		} else {
			return usageErr("--scope: %v", err)
		}
	}
	if *activeOnly {
		p.Statuses = []memory.Status{memory.StatusActive, memory.StatusProposed}
	}
	rt, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()

	w := c.out
	if *out != "-" {
		// Memories are private: the file is readable by its owner only, and an
		// existing file is never overwritten. A failed export removes it.
		f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create %s: %w", *out, err)
		}
		defer func() {
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				os.Remove(*out)
			}
		}()
		w = f
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	if err := enc.Encode(store.ArchiveHeader{Format: store.ArchiveFormat, Exported: time.Now().UTC(), Version: buildinfo.Version}); err != nil {
		return err
	}
	counts, err := rt.store.Export(ctx, p, func(r store.ArchiveRecord) error { return enc.Encode(r) })
	if err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	dest := *out
	if dest == "-" {
		dest = "stdout"
	}
	fmt.Fprintf(c.errOut, "Exported %d memories, %d edges, %d code references to %s.\n", counts.Memories, counts.Edges, counts.Refs, dest)
	return nil
}

// importArchive reads an archive from a file or stdin.
func (c *cli) importArchive(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "check the archive and report what would be imported, without writing")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: kenfold import [--dry-run] <archive.jsonl | ->")
	}
	var r io.Reader = c.in
	if pos[0] != "-" {
		f, err := os.Open(pos[0])
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxArchiveLine)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return err
		}
		return store.ErrArchiveFormat
	}
	var h store.ArchiveHeader
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil || h.Format != store.ArchiveFormat {
		return fmt.Errorf("%w (expected %s)", store.ErrArchiveFormat, store.ArchiveFormat)
	}
	// Read and validate everything before writing anything.
	var recs []store.ArchiveRecord
	line := 1
	for sc.Scan() {
		line++
		var rec store.ArchiveRecord
		dec := json.NewDecoder(strings.NewReader(sc.Text()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rec); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		if err := validRecord(rec); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		recs = append(recs, rec)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("line %d: %w", line+1, err)
	}
	// The last record must be the end record, and its counts must match:
	// otherwise the archive was truncated (a failed export, a partial copy).
	var got store.ArchiveCounts
	var end *store.ArchiveCounts
	for i, rec := range recs {
		switch {
		case rec.End != nil:
			if i != len(recs)-1 {
				return errors.New("the archive has records after its end record")
			}
			end = rec.End
		case rec.Memory != nil:
			got.Memories++
		case rec.Edge != nil:
			got.Edges++
		default:
			got.Refs++
		}
	}
	if end == nil || *end != got {
		return fmt.Errorf("the archive is incomplete (no end record, or its counts do not match: found %d memories, %d edges, %d references)",
			got.Memories, got.Edges, got.Refs)
	}
	mems, edges, refs := got.Memories, got.Edges, got.Refs
	if *dryRun {
		fmt.Fprintf(c.out, "Archive from %s (Kenfold %s): %d memories, %d edges, %d code references. Nothing was written.\n",
			h.Exported.Format(time.RFC3339), h.Version, mems, edges, refs)
		return nil
	}
	rt, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()
	sum, err := rt.store.Import(ctx, func(im *store.Importer) error {
		for _, rec := range recs {
			if err := im.Add(ctx, rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("import failed, nothing was written: %w", err)
	}
	fmt.Fprintf(c.out, "Imported %d memories, %d edges, %d code references", sum.Memories, sum.Edges, sum.Refs)
	if sum.Existing > 0 {
		fmt.Fprintf(c.out, "; %d memories were already here and were left unchanged", sum.Existing)
	}
	fmt.Fprintln(c.out, ".")
	if sum.Memories > 0 {
		fmt.Fprintln(c.out, "Embeddings are computed by the server in the background (or run `kenfold reindex`).")
	}
	return nil
}

// validRecord checks a record against the invariants the database enforces,
// so a bad archive fails with a line number instead of a constraint name,
// and rejects content with credentials, like remember does.
func validRecord(r store.ArchiveRecord) error {
	set := 0
	for _, b := range []bool{r.Memory != nil, r.Edge != nil, r.Ref != nil, r.End != nil} {
		if b {
			set++
		}
	}
	if set != 1 {
		return errors.New("each record must hold exactly one of memory, edge, ref, end")
	}
	switch {
	case r.Memory != nil:
		m := r.Memory
		if !store.ValidID(m.ID) {
			return fmt.Errorf("memory id %q is not a UUID", m.ID)
		}
		if !m.Type.Valid() {
			return fmt.Errorf("memory %s: unknown type %q", m.ID, m.Type)
		}
		if _, err := memory.Scope(strings.TrimPrefix(strings.TrimPrefix(m.Scope, "project:"), "repo:")); err != nil && m.Scope != memory.ScopeUser {
			return fmt.Errorf("memory %s: invalid scope %q", m.ID, m.Scope)
		}
		if strings.TrimSpace(m.Content) == "" {
			return fmt.Errorf("memory %s: empty content", m.ID)
		}
		if fs := secrets.Scan(m.Content); len(fs) > 0 {
			return fmt.Errorf("memory %s contains a %s; remove it (kenfold scan --redact on the source) and export again", m.ID, fs[0].Label)
		}
		if m.Type == memory.TypeTemporary && m.ExpiresAt == nil {
			return fmt.Errorf("memory %s: temporary memories need expires_at", m.ID)
		}
		if !memory.ValidAgent(m.SourceAgent) {
			return fmt.Errorf("memory %s: invalid source_agent %q", m.ID, m.SourceAgent)
		}
	case r.Edge != nil:
		if !store.ValidID(r.Edge.Src) || !store.ValidID(r.Edge.Dst) || r.Edge.Relation == "" || r.Edge.Src == r.Edge.Dst {
			return errors.New("invalid edge")
		}
	case r.Ref != nil:
		if !store.ValidID(r.Ref.MemoryID) || (r.Ref.Path == "" && r.Ref.Symbol == "") {
			return errors.New("invalid code reference")
		}
	case r.End != nil:
		if r.End.Memories < 0 || r.End.Edges < 0 || r.End.Refs < 0 {
			return errors.New("invalid end record")
		}
	}
	return nil
}
