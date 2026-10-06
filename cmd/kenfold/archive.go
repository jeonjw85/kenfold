package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
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
	if err := decodeArchiveLine(sc.Text(), &h); err != nil || h.Format != store.ArchiveFormat {
		return fmt.Errorf("%w (expected %s)", store.ErrArchiveFormat, store.ArchiveFormat)
	}
	// Read and validate everything before writing anything.
	var recs []store.ArchiveRecord
	line := 1
	for sc.Scan() {
		line++
		var rec store.ArchiveRecord
		if err := decodeArchiveLine(sc.Text(), &rec); err != nil {
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

func decodeArchiveLine(line string, out any) error {
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("each archive line must contain exactly one JSON value")
	}
	return nil
}

func validArchiveScope(scope string) bool {
	if scope == memory.ScopeUser {
		return true
	}
	for _, prefix := range []string{"project:", "repo:"} {
		if project, ok := strings.CutPrefix(scope, prefix); ok && project != "" {
			normalized, err := memory.Scope(project)
			return err == nil && normalized == "project:"+project
		}
	}
	return false
}

// validRecord checks a record against the invariants the database enforces,
// so a bad archive fails with a line number instead of a constraint name,
// and rejects credentials in content and metadata, like remember does.
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
	// Inspect decoded strings, not escaped JSON: credentials can also live in
	// nested attrs, provenance, and code references. Check before diagnostics
	// that might quote an invalid field containing a credential.
	b, err := json.Marshal(r)
	if err != nil {
		return errors.New("record is not valid JSON")
	}
	var value any
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	if label := archiveSecret(value); label != "" {
		return fmt.Errorf("record contains a %s; remove it and export again", label)
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
		if !validArchiveScope(m.Scope) {
			return fmt.Errorf("memory %s: invalid scope %q", m.ID, m.Scope)
		}
		switch m.Trust {
		case memory.TrustUser, memory.TrustAgent, memory.TrustExternal:
		default:
			return fmt.Errorf("memory %s: invalid trust %q", m.ID, m.Trust)
		}
		switch m.Status {
		case memory.StatusProposed, memory.StatusActive, memory.StatusSuperseded, memory.StatusDeleted:
		default:
			return fmt.Errorf("memory %s: invalid status %q", m.ID, m.Status)
		}
		if math.IsNaN(m.Confidence) || m.Confidence < 0 || m.Confidence > 1 {
			return fmt.Errorf("memory %s: confidence must be between 0 and 1", m.ID)
		}
		if m.Supersedes != nil && (!store.ValidID(*m.Supersedes) || strings.EqualFold(*m.Supersedes, m.ID)) {
			return fmt.Errorf("memory %s: invalid supersedes id", m.ID)
		}
		if m.ValidFrom != nil && m.ValidTo != nil && m.ValidTo.Before(*m.ValidFrom) {
			return fmt.Errorf("memory %s: valid_to is before valid_from", m.ID)
		}
		if strings.TrimSpace(m.Content) == "" {
			return fmt.Errorf("memory %s: empty content", m.ID)
		}
		if m.Type == memory.TypeTemporary && m.ExpiresAt == nil {
			return fmt.Errorf("memory %s: temporary memories need expires_at", m.ID)
		}
		if !memory.ValidAgent(m.SourceAgent) {
			return fmt.Errorf("memory %s: invalid source_agent %q", m.ID, m.SourceAgent)
		}
	case r.Edge != nil:
		if !store.ValidID(r.Edge.Src) || !store.ValidID(r.Edge.Dst) || r.Edge.Relation == "" || strings.EqualFold(r.Edge.Src, r.Edge.Dst) {
			return errors.New("invalid edge")
		}
		if !memory.ValidAgent(r.Edge.SourceAgent) || math.IsNaN(r.Edge.Weight) || math.IsInf(r.Edge.Weight, 0) {
			return errors.New("invalid edge provenance or weight")
		}
	case r.Ref != nil:
		if !store.ValidID(r.Ref.MemoryID) || (r.Ref.Path == "" && r.Ref.Symbol == "") {
			return errors.New("invalid code reference")
		}
		if !validArchiveScope(r.Ref.Scope) || r.Ref.Scope == memory.ScopeUser {
			return errors.New("code references need a project or repo scope")
		}
		switch r.Ref.State {
		case "pending", "current", "changed", "missing", "unresolved":
		default:
			return errors.New("invalid code reference state")
		}
		if (r.Ref.AnchorHash == nil) != (r.Ref.AnchorCommit == nil) {
			return errors.New("code reference anchor hash and commit must be set together")
		}
	case r.End != nil:
		if r.End.Memories < 0 || r.End.Edges < 0 || r.End.Refs < 0 {
			return errors.New("invalid end record")
		}
	}
	return nil
}

func archiveSecret(value any, fields ...string) string {
	switch v := value.(type) {
	case string:
		if fs := secrets.Scan(v); len(fs) > 0 {
			return fs[0].Label
		}
		// Preserve secret-named ancestors through containers, e.g.
		// {"api_key":[{"value":"<random-looking credential>"}]}.
		for _, field := range fields {
			if fs := secrets.Scan(field + "=" + v); len(fs) > 0 {
				return fs[0].Label
			}
		}
	case []any:
		for _, item := range v {
			if label := archiveSecret(item, fields...); label != "" {
				return label
			}
		}
	case map[string]any:
		for key, item := range v {
			if label := archiveSecret(key); label != "" {
				return label
			}
			if label := archiveSecret(item, append(fields, key)...); label != "" {
				return label
			}
		}
	}
	return ""
}
