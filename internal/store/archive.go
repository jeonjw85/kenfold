package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kenfold/kenfold/internal/memory"
)

// Export and import move memories between Kenfold installs, or into a file
// for backup. The archive is JSON Lines: a header, then one record per line.
// It holds what the memories are, not how they are indexed: embeddings are
// left out (the target re-embeds with its own model) and so are
// extraction job records. Code references keep their anchors, since the
// commits they name exist in the repository, not in Kenfold. API keys and
// OAuth grants are credentials of one install and are never exported.

// ArchiveFormat identifies the archive format; bump it on incompatible changes.
const ArchiveFormat = "kenfold-archive/1"

// ArchiveHeader is the first line of an archive.
type ArchiveHeader struct {
	Format   string    `json:"format"`
	Exported time.Time `json:"exported_at"`
	Version  string    `json:"kenfold_version"`
}

// ArchiveCounts is the last record of a complete archive; importing checks
// it, so a truncated archive is refused.
type ArchiveCounts struct {
	Memories int `json:"memories"`
	Edges    int `json:"edges"`
	Refs     int `json:"refs"`
}

// ArchiveRecord is one line after the header; exactly one field is set.
type ArchiveRecord struct {
	Memory *ArchivedMemory `json:"memory,omitempty"`
	Edge   *ArchivedEdge   `json:"edge,omitempty"`
	Ref    *ArchivedRef    `json:"ref,omitempty"`
	End    *ArchiveCounts  `json:"end,omitempty"`
}

// ArchivedMemory is a memory row without its embedding.
type ArchivedMemory struct {
	ID            string         `json:"id"`
	Type          memory.Type    `json:"type"`
	Scope         string         `json:"scope"`
	Content       string         `json:"content"`
	Attrs         map[string]any `json:"attrs,omitempty"`
	SourceAgent   string         `json:"source_agent"`
	SourceSession *string        `json:"source_session,omitempty"`
	EvidenceURI   *string        `json:"evidence_uri,omitempty"`
	Trust         memory.Trust   `json:"trust"`
	Confidence    float64        `json:"confidence"`
	Status        memory.Status  `json:"status"`
	Supersedes    *string        `json:"supersedes,omitempty"`
	ValidFrom     *time.Time     `json:"valid_from,omitempty"`
	ValidTo       *time.Time     `json:"valid_to,omitempty"`
	ExpiresAt     *time.Time     `json:"expires_at,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// ArchivedEdge is a memory_edge row.
type ArchivedEdge struct {
	Src         string    `json:"src"`
	Dst         string    `json:"dst"`
	Relation    string    `json:"relation"`
	Weight      float64   `json:"weight"`
	SourceAgent string    `json:"source_agent"`
	CreatedAt   time.Time `json:"created_at"`
}

// ArchivedRef is a memory_ref row.
type ArchivedRef struct {
	MemoryID      string     `json:"memory_id"`
	Scope         string     `json:"scope"`
	Path          string     `json:"path,omitempty"`
	Symbol        string     `json:"symbol,omitempty"`
	State         string     `json:"state"`
	ResolvedPath  *string    `json:"resolved_path,omitempty"`
	AnchorHash    *string    `json:"anchor_hash,omitempty"`
	AnchorCommit  *string    `json:"anchor_commit,omitempty"`
	CheckedCommit *string    `json:"checked_commit,omitempty"`
	CheckedAt     *time.Time `json:"checked_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// ExportParams selects what Export writes.
type ExportParams struct {
	Scopes   []string        // empty = all scopes
	Statuses []memory.Status // empty = all statuses (history included)
}

// exportSel selects the exported memories ($1 scopes, $2 statuses; NULL = all).
const exportSel = `($1::text[] IS NULL OR scope = ANY($1)) AND ($2::text[] IS NULL OR status = ANY($2))`

// Export streams memories (in id order, which is creation order), then the
// edges and code references between exported memories, to emit. It runs in
// one read-only repeatable-read transaction, so the archive is a consistent
// snapshot and export never writes.
// The records are followed by an End record with the counts.
func (s *Store) Export(ctx context.Context, p ExportParams, emit func(ArchiveRecord) error) (ArchiveCounts, error) {
	var h ArchiveCounts
	scopes, statuses := nilIfEmpty(p.Scopes), stringsOf(p.Statuses)
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		each := func(q string, scanEmit func(pgx.Rows) error) error {
			rows, err := tx.Query(ctx, q, scopes, statuses)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				if err := scanEmit(rows); err != nil {
					return err
				}
			}
			return rows.Err()
		}
		err := each(`SELECT `+columns+` FROM memory WHERE `+exportSel+` ORDER BY id`, func(r pgx.Rows) error {
			var m Memory
			if err := scan(r, &m); err != nil {
				return err
			}
			a := ArchivedMemory{ID: m.ID, Type: m.Type, Scope: m.Scope, Content: m.Content, Attrs: m.Attrs, SourceAgent: m.SourceAgent,
				SourceSession: m.SourceSession, EvidenceURI: m.EvidenceURI, Trust: m.Trust, Confidence: m.Confidence, Status: m.Status,
				Supersedes: m.Supersedes, ValidFrom: m.ValidFrom, ValidTo: m.ValidTo, ExpiresAt: m.ExpiresAt, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
			if len(a.Attrs) == 0 {
				a.Attrs = nil
			}
			h.Memories++
			return emit(ArchiveRecord{Memory: &a})
		})
		if err != nil {
			return err
		}
		err = each(`SELECT e.src::text, e.dst::text, e.relation, e.weight, e.source_agent, e.created_at FROM memory_edge e
			WHERE e.src IN (SELECT id FROM memory WHERE `+exportSel+`) AND e.dst IN (SELECT id FROM memory WHERE `+exportSel+`)
			ORDER BY e.src, e.relation, e.dst`, func(r pgx.Rows) error {
			var e ArchivedEdge
			if err := r.Scan(&e.Src, &e.Dst, &e.Relation, &e.Weight, &e.SourceAgent, &e.CreatedAt); err != nil {
				return err
			}
			h.Edges++
			return emit(ArchiveRecord{Edge: &e})
		})
		if err != nil {
			return err
		}
		err = each(`SELECT r.memory_id::text, r.scope, r.path, r.symbol, r.state, r.resolved_path, r.anchor_hash, r.anchor_commit,
			       r.checked_commit, r.checked_at, r.created_at
			FROM memory_ref r WHERE r.memory_id IN (SELECT id FROM memory WHERE `+exportSel+`)
			ORDER BY r.memory_id, r.path, r.symbol`, func(r pgx.Rows) error {
			var f ArchivedRef
			if err := r.Scan(&f.MemoryID, &f.Scope, &f.Path, &f.Symbol, &f.State, &f.ResolvedPath, &f.AnchorHash, &f.AnchorCommit,
				&f.CheckedCommit, &f.CheckedAt, &f.CreatedAt); err != nil {
				return err
			}
			h.Refs++
			return emit(ArchiveRecord{Ref: &f})
		})
		if err != nil {
			return err
		}
		end := h
		return emit(ArchiveRecord{End: &end})
	})
	return h, err
}

// ImportSummary reports what Import did.
type ImportSummary struct {
	Memories, Edges, Refs int // inserted
	Existing              int // memories already present (same id), left unchanged
	Skipped               int // edges and refs whose memories are not in the target
}

// ErrArchiveFormat is returned for archives Kenfold cannot read.
var ErrArchiveFormat = errors.New("not a Kenfold archive, or a newer format")

// Importer writes archive records into the store in one transaction. Records
// must come in archive order (memories, then edges and refs). Memories that
// already exist (same id) are left unchanged, so importing the same archive
// twice is harmless; everything else is inserted as it was, including ids,
// statuses, and timestamps. Embeddings are filled in by the backfill.
type Importer struct {
	tx      pgx.Tx
	sum     ImportSummary
	pending []ArchivedMemory // memories whose supersedes target comes later
}

// Import runs fn with an Importer and commits if fn succeeds.
func (s *Store) Import(ctx context.Context, fn func(*Importer) error) (ImportSummary, error) {
	var sum ImportSummary
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		im := &Importer{tx: tx}
		if err := fn(im); err != nil {
			return err
		}
		if err := im.finish(ctx); err != nil {
			return err
		}
		sum = im.sum
		return nil
	})
	return sum, err
}

// Add imports one record.
func (im *Importer) Add(ctx context.Context, r ArchiveRecord) error {
	switch {
	case r.Memory != nil:
		return im.addMemory(ctx, *r.Memory)
	case r.Edge != nil:
		e := r.Edge
		if !ValidID(e.Src) || !ValidID(e.Dst) {
			return ErrInvalidID
		}
		tag, err := im.tx.Exec(ctx, `
			INSERT INTO memory_edge (src, dst, relation, weight, source_agent, created_at)
			SELECT $1, $2, $3, $4, $5, $6
			WHERE EXISTS (SELECT 1 FROM memory WHERE id = $1) AND EXISTS (SELECT 1 FROM memory WHERE id = $2)
			ON CONFLICT DO NOTHING`, e.Src, e.Dst, e.Relation, e.Weight, e.SourceAgent, e.CreatedAt)
		if err != nil {
			return fmt.Errorf("import edge: %w", err)
		}
		if tag.RowsAffected() == 1 {
			im.sum.Edges++
		} else {
			im.sum.Skipped++
		}
	case r.End != nil:
		return nil
	case r.Ref != nil:
		f := r.Ref
		if !ValidID(f.MemoryID) {
			return ErrInvalidID
		}
		tag, err := im.tx.Exec(ctx, `
			INSERT INTO memory_ref (memory_id, scope, path, symbol, state, resolved_path, anchor_hash, anchor_commit, checked_commit, checked_at, created_at)
			SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11 WHERE EXISTS (SELECT 1 FROM memory WHERE id = $1)
			ON CONFLICT DO NOTHING`,
			f.MemoryID, f.Scope, f.Path, f.Symbol, f.State, f.ResolvedPath, f.AnchorHash, f.AnchorCommit, f.CheckedCommit, f.CheckedAt, f.CreatedAt)
		if err != nil {
			return fmt.Errorf("import code reference: %w", err)
		}
		if tag.RowsAffected() == 1 {
			im.sum.Refs++
		} else {
			im.sum.Skipped++
		}
	default:
		return errors.New("empty archive record")
	}
	return nil
}

func (im *Importer) addMemory(ctx context.Context, m ArchivedMemory) error {
	if !ValidID(m.ID) || (m.Supersedes != nil && !ValidID(*m.Supersedes)) {
		return ErrInvalidID
	}
	sup := m.Supersedes
	if sup != nil {
		// The replaced memory is older, so it normally precedes this one;
		// if it is not there yet, link it at the end.
		var ok bool
		if err := im.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memory WHERE id = $1)`, *sup).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			im.pending = append(im.pending, m)
			sup = nil
		}
	}
	attrs := m.Attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	tag, err := im.tx.Exec(ctx, `
		INSERT INTO memory (id, type, scope, content, attrs, source_agent, source_session, evidence_uri, trust, confidence,
		                    status, supersedes, valid_from, valid_to, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (id) DO NOTHING`,
		m.ID, m.Type, m.Scope, m.Content, attrs, m.SourceAgent, m.SourceSession, m.EvidenceURI, m.Trust, m.Confidence,
		m.Status, sup, m.ValidFrom, m.ValidTo, m.ExpiresAt, m.CreatedAt, m.UpdatedAt)
	if err != nil {
		return fmt.Errorf("import memory %s: %w", m.ID, err)
	}
	if tag.RowsAffected() == 1 {
		im.sum.Memories++
	} else {
		im.sum.Existing++
	}
	return nil
}

// finish links supersedes pointers that arrived before their target and
// restores updated_at (the touch trigger overwrites it on update).
func (im *Importer) finish(ctx context.Context) error {
	for _, m := range im.pending {
		if _, err := im.tx.Exec(ctx, `
			UPDATE memory SET supersedes = $2, updated_at = $3
			WHERE id = $1 AND supersedes IS NULL AND EXISTS (SELECT 1 FROM memory WHERE id = $2)`, m.ID, *m.Supersedes, m.UpdatedAt); err != nil {
			return fmt.Errorf("link supersedes: %w", err)
		}
	}
	return nil
}
