package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/migrations"
)

func TestArchiveStoreIntegration(t *testing.T) {
	url := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := New(pool)
	reset := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `TRUNCATE memory CASCADE`); err != nil {
			t.Fatal(err)
		}
	}
	const childID = "11111111-1111-1111-1111-111111111111"
	const parentID = "22222222-2222-2222-2222-222222222222"
	const ancestorID = "33333333-3333-3333-3333-333333333333"
	at := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	mk := func(id string, supersedes *string) ArchivedMemory {
		return ArchivedMemory{ID: id, Type: memory.TypeProject, Scope: "project:archive-test", Content: "fact " + id,
			SourceAgent: "test", Trust: memory.TrustAgent, Confidence: 0.5, Status: memory.StatusActive,
			Supersedes: supersedes, CreatedAt: at, UpdatedAt: at.Add(time.Hour)}
	}
	importRecords := func(records ...ArchiveRecord) (ImportSummary, error) {
		return s.Import(ctx, func(im *Importer) error {
			for _, rec := range records {
				if err := im.Add(ctx, rec); err != nil {
					return err
				}
			}
			return nil
		})
	}

	t.Run("forward supersedes preserves timestamps and graph", func(t *testing.T) {
		reset()
		parent, ancestor := parentID, ancestorID
		child, middle, old := mk(childID, &parent), mk(parentID, &ancestor), mk(ancestorID, nil)
		edge := ArchivedEdge{Src: childID, Dst: parentID, Relation: "replaces", Weight: 1, SourceAgent: "test", CreatedAt: at}
		ref := ArchivedRef{MemoryID: childID, Scope: child.Scope, Path: "main.go", State: RefPending, CreatedAt: at}
		sum, err := importRecords(ArchiveRecord{Memory: &child}, ArchiveRecord{Memory: &middle}, ArchiveRecord{Memory: &old},
			ArchiveRecord{Edge: &edge}, ArchiveRecord{Ref: &ref})
		if err != nil || sum.Memories != 3 || sum.Edges != 1 || sum.Refs != 1 {
			t.Fatalf("import = %+v, %v", sum, err)
		}
		for _, original := range []ArchivedMemory{child, middle, old} {
			got, err := s.Get(ctx, original.ID)
			if err != nil || !got.CreatedAt.Equal(original.CreatedAt) || !got.UpdatedAt.Equal(original.UpdatedAt) {
				t.Errorf("timestamps for %s = %v, %v, %v", original.ID, got.CreatedAt, got.UpdatedAt, err)
			}
			if original.Supersedes != nil && (got.Supersedes == nil || *got.Supersedes != *original.Supersedes) {
				t.Errorf("supersedes for %s = %v", original.ID, got.Supersedes)
			}
		}
	})

	t.Run("existing memory is untouched when target arrives later", func(t *testing.T) {
		reset()
		existing := mk(childID, nil)
		if _, err := importRecords(ArchiveRecord{Memory: &existing}); err != nil {
			t.Fatal(err)
		}
		parent := parentID
		incoming, old := mk(childID, &parent), mk(parentID, nil)
		incoming.Content, incoming.UpdatedAt = "different imported fact", at.Add(2*time.Hour)
		sum, err := importRecords(ArchiveRecord{Memory: &incoming}, ArchiveRecord{Memory: &old})
		if err != nil || sum.Existing != 1 || sum.Memories != 1 {
			t.Fatalf("import = %+v, %v", sum, err)
		}
		got, err := s.Get(ctx, childID)
		if err != nil || got.Content != existing.Content || got.Supersedes != nil || !got.UpdatedAt.Equal(existing.UpdatedAt) {
			t.Errorf("existing memory changed: %+v, %v", got, err)
		}
	})

	t.Run("forward supersedes matches UUIDs case insensitively", func(t *testing.T) {
		reset()
		parent := "bbbbbbbb-2222-2222-2222-222222222222"
		ancestor := "cccccccc-3333-3333-3333-333333333333"
		child := mk(childID, &parent)
		middle := mk("BBBBBBBB-2222-2222-2222-222222222222", &ancestor)
		old := mk(ancestor, nil)
		if sum, err := importRecords(ArchiveRecord{Memory: &child}, ArchiveRecord{Memory: &middle}, ArchiveRecord{Memory: &old}); err != nil || sum.Memories != 3 {
			t.Fatalf("import = %+v, %v", sum, err)
		}
		for _, original := range []ArchivedMemory{child, middle} {
			got, err := s.Get(ctx, original.ID)
			if err != nil || got.Supersedes == nil || *got.Supersedes != *original.Supersedes || !got.UpdatedAt.Equal(original.UpdatedAt) {
				t.Errorf("mixed-case history changed: %+v, %v", got, err)
			}
		}
	})

	t.Run("filtered-out supersedes target is omitted", func(t *testing.T) {
		reset()
		parent := parentID
		child := mk(childID, &parent)
		if sum, err := importRecords(ArchiveRecord{Memory: &child}); err != nil || sum.Memories != 1 {
			t.Fatalf("import = %+v, %v", sum, err)
		}
		got, err := s.Get(ctx, childID)
		if err != nil || got.Supersedes != nil || !got.UpdatedAt.Equal(child.UpdatedAt) {
			t.Errorf("filtered import = %+v, %v", got, err)
		}
	})

	t.Run("supersedes cycle rolls back", func(t *testing.T) {
		reset()
		parent, child := parentID, childID
		a, b := mk(childID, &parent), mk(parentID, &child)
		if _, err := importRecords(ArchiveRecord{Memory: &a}, ArchiveRecord{Memory: &b}); err == nil {
			t.Fatal("cyclic history accepted")
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM memory`).Scan(&n); err != nil || n != 0 {
			t.Errorf("failed import wrote %d memories: %v", n, err)
		}
	})

	t.Run("code reference scope mismatch rolls back", func(t *testing.T) {
		reset()
		m := mk(childID, nil)
		ref := ArchivedRef{MemoryID: childID, Scope: "project:another-project", Path: "main.go", State: RefPending, CreatedAt: at}
		if _, err := importRecords(ArchiveRecord{Memory: &m}, ArchiveRecord{Ref: &ref}); err == nil {
			t.Fatal("code reference in another scope accepted")
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM memory`).Scan(&n); err != nil || n != 0 {
			t.Errorf("failed import wrote %d memories: %v", n, err)
		}
	})
}
