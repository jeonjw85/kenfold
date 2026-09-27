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

// TestStoreIntegration exercises the store against a real PostgreSQL+pgvector
// database. It is opt-in because it writes to the schema:
//
//	KENFOLD_TEST_DATABASE_URL=postgres://... go test -run Integration ./internal/store/
//
// `make test-integration` points it at a throwaway database in compose Postgres.
func TestStoreIntegration(t *testing.T) {
	url := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	// Ensure the schema exists (idempotent).
	if _, err := migrations.Up(ctx, url); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	// Isolate this run's rows so the test is repeatable without a reset.
	agent := "store-test-" + time.Now().Format("150405.000")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM memory WHERE source_agent = $1`, agent)
	})

	s := New(pool)

	t.Run("create defaults", func(t *testing.T) {
		m, err := s.Create(ctx, CreateParams{
			Type:        memory.TypeSemantic,
			Scope:       "user",
			Content:     "Go generics landed in 1.18",
			SourceAgent: agent,
			Trust:       memory.TrustAgent,
			Confidence:  0.5,
			Status:      memory.StatusActive,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if m.ID == "" {
			t.Error("id not returned")
		}
		if m.Status != memory.StatusActive {
			t.Errorf("status = %q, want active", m.Status)
		}
		if m.CreatedAt.IsZero() || m.Attrs == nil {
			t.Errorf("created_at/attrs not populated: %+v", m)
		}

		got, err := s.Get(ctx, m.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Content != m.Content {
			t.Errorf("round-trip content = %q", got.Content)
		}
	})

	t.Run("temporary with ttl", func(t *testing.T) {
		exp := time.Now().Add(time.Hour)
		m, err := s.Create(ctx, CreateParams{
			Type:        memory.TypeTemporary,
			Scope:       "user",
			Content:     "WIP: refactoring the store layer",
			SourceAgent: agent,
			Trust:       memory.TrustAgent,
			Confidence:  0.5,
			Status:      memory.StatusActive,
			ExpiresAt:   &exp,
		})
		if err != nil {
			t.Fatalf("create temporary: %v", err)
		}
		if m.ExpiresAt == nil {
			t.Error("expires_at not persisted")
		}
	})

	t.Run("temporary without ttl is rejected by the db", func(t *testing.T) {
		_, err := s.Create(ctx, CreateParams{
			Type:        memory.TypeTemporary,
			Scope:       "user",
			Content:     "no ttl",
			SourceAgent: agent,
			Trust:       memory.TrustAgent,
			Confidence:  0.5,
			Status:      memory.StatusActive,
		})
		if err == nil {
			t.Error("create succeeded; want constraint violation")
		}
	})

	t.Run("supersede marks the old memory superseded", func(t *testing.T) {
		old, err := s.Create(ctx, CreateParams{
			Type: memory.TypeProject, Scope: "project:example.com/a/b",
			Content: "We deploy on Fridays", SourceAgent: agent,
			Trust: memory.TrustAgent, Confidence: 0.5, Status: memory.StatusActive,
		})
		if err != nil {
			t.Fatalf("create old: %v", err)
		}

		newM, err := s.Create(ctx, CreateParams{
			Type: memory.TypeProject, Scope: "project:example.com/a/b",
			Content: "We no longer deploy on Fridays", SourceAgent: agent,
			Trust: memory.TrustAgent, Confidence: 0.5, Status: memory.StatusActive,
			Supersedes: &old.ID,
		})
		if err != nil {
			t.Fatalf("create new: %v", err)
		}
		if newM.Supersedes == nil || *newM.Supersedes != old.ID {
			t.Errorf("supersedes = %v, want %s", newM.Supersedes, old.ID)
		}

		reloaded, err := s.Get(ctx, old.ID)
		if err != nil {
			t.Fatalf("get old: %v", err)
		}
		if reloaded.Status != memory.StatusSuperseded {
			t.Errorf("old status = %q, want superseded", reloaded.Status)
		}
	})

	t.Run("supersede of a missing memory fails", func(t *testing.T) {
		missing := "00000000-0000-0000-0000-000000000000"
		_, err := s.Create(ctx, CreateParams{
			Type: memory.TypeSemantic, Scope: "user", Content: "x",
			SourceAgent: agent, Trust: memory.TrustAgent, Confidence: 0.5,
			Status: memory.StatusActive, Supersedes: &missing,
		})
		if err == nil {
			t.Error("create succeeded; want ErrNotFound")
		}
	})

	t.Run("soft delete is idempotent and records reason", func(t *testing.T) {
		m, err := s.Create(ctx, CreateParams{
			Type: memory.TypeSemantic, Scope: "user", Content: "wrong fact",
			SourceAgent: agent, Trust: memory.TrustAgent, Confidence: 0.5,
			Status: memory.StatusActive,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		d1, err := s.SoftDelete(ctx, m.ID, "outdated")
		if err != nil {
			t.Fatalf("soft delete: %v", err)
		}
		if d1.Status != memory.StatusDeleted {
			t.Errorf("status = %q, want deleted", d1.Status)
		}
		if d1.Attrs["forget_reason"] != "outdated" {
			t.Errorf("forget_reason = %v, want outdated", d1.Attrs["forget_reason"])
		}

		// Second delete must not error (idempotent).
		d2, err := s.SoftDelete(ctx, m.ID, "")
		if err != nil {
			t.Fatalf("second soft delete: %v", err)
		}
		if d2.Status != memory.StatusDeleted {
			t.Errorf("second status = %q, want deleted", d2.Status)
		}
	})

	t.Run("get and delete of unknown id return ErrNotFound", func(t *testing.T) {
		missing := "11111111-1111-1111-1111-111111111111"
		if _, err := s.Get(ctx, missing); err != ErrNotFound {
			t.Errorf("get err = %v, want ErrNotFound", err)
		}
		if _, err := s.SoftDelete(ctx, missing, ""); err != ErrNotFound {
			t.Errorf("delete err = %v, want ErrNotFound", err)
		}
	})
}
