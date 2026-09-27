package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/migrations"
)

// TestExtractionStoreIntegration covers the extraction job table and the
// helpers the extractor uses. It TRUNCATES memory; use a throwaway database.
func TestExtractionStoreIntegration(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `TRUNCATE memory CASCADE`); err != nil {
		t.Fatal(err)
	}
	s := New(pool)
	mk := func(typ memory.Type, scope, content string, status memory.Status) Memory {
		t.Helper()
		m, err := s.Create(ctx, CreateParams{Type: typ, Scope: scope, Content: content, SourceAgent: "t",
			Trust: memory.TrustAgent, Confidence: 0.5, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	future := time.Now().Add(time.Hour)

	older := mk(memory.TypeEpisodic, "project:ex/x", "session one", memory.StatusActive)
	time.Sleep(5 * time.Millisecond)
	newer := mk(memory.TypeEpisodic, "project:ex/x", "session two", memory.StatusActive)
	mk(memory.TypeProject, "project:ex/x", "not a session", memory.StatusActive)
	gone := mk(memory.TypeEpisodic, "project:ex/x", "deleted session", memory.StatusActive)
	s.SoftDelete(ctx, gone.ID, "", "t")

	// Summaries newer than notBefore are left alone (the session may still be ending).
	if _, err := s.ClaimExtraction(ctx, older.CreatedAt, time.Minute, "m"); !errors.Is(err, ErrNotFound) {
		t.Errorf("claim before notBefore: %v", err)
	}
	j1, err := s.ClaimExtraction(ctx, future, time.Minute, "m")
	if err != nil || j1.Source.ID != older.ID || j1.Attempts != 1 {
		t.Fatalf("first claim = %v (%d), %v; want oldest", j1.Source.Content, j1.Attempts, err)
	}
	j2, err := s.ClaimExtraction(ctx, future, time.Minute, "m")
	if err != nil || j2.Source.ID != newer.ID {
		t.Fatalf("second claim = %v, %v", j2.Source.Content, err)
	}
	if _, err := s.ClaimExtraction(ctx, future, time.Minute, "m"); !errors.Is(err, ErrNotFound) {
		t.Errorf("third claim: %v (deleted and non-episodic must not be claimed)", err)
	}

	if err := s.FinishExtraction(ctx, j1.Source.ID, 3, 2); err != nil {
		t.Fatal(err)
	}
	// Failed with retry in the past: claimable again with attempts incremented.
	past := time.Now().Add(-time.Second)
	if err := s.FailExtraction(ctx, j2.Source.ID, "model timeout", &past); err != nil {
		t.Fatal(err)
	}
	j3, err := s.ClaimExtraction(ctx, future, time.Minute, "m2")
	if err != nil || j3.Source.ID != newer.ID || j3.Attempts != 2 {
		t.Fatalf("retry claim = %v attempts %d, %v", j3.Source.Content, j3.Attempts, err)
	}
	// Failed without retry: never claimed again.
	if err := s.FailExtraction(ctx, j3.Source.ID, "bad output", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimExtraction(ctx, future, time.Minute, "m"); !errors.Is(err, ErrNotFound) {
		t.Errorf("permanently failed claimed again: %v", err)
	}
	// An expired lease (crashed worker) is reclaimed.
	crashed := mk(memory.TypeEpisodic, "user", "crashed session", memory.StatusActive)
	jc, err := s.ClaimExtraction(ctx, future, -time.Second, "m")
	if err != nil || jc.Source.ID != crashed.ID {
		t.Fatalf("claim crashed = %v, %v", jc.Source.Content, err)
	}
	jc2, err := s.ClaimExtraction(ctx, future, time.Minute, "m")
	if err != nil || jc2.Source.ID != crashed.ID || jc2.Attempts != 2 {
		t.Errorf("expired lease not reclaimed: %v %d %v", jc2.Source.Content, jc2.Attempts, err)
	}
	if err := s.FinishExtraction(ctx, "11111111-1111-1111-1111-111111111111", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("finish unknown: %v", err)
	}
	st, err := s.ExtractionStatus(ctx)
	if err != nil || st.Done != 1 || st.Failed != 1 || st.Running != 1 || st.Pending != 0 || st.Stored != 2 {
		t.Errorf("status = %+v, %v", st, err)
	}

	// SeenContent finds memories in any status.
	rej := mk(memory.TypeProject, "project:ex/x", "We use pnpm only.", memory.StatusProposed)
	s.SoftDelete(ctx, rej.ID, "user rejected", "kenfold-cli")
	if m, err := s.SeenContent(ctx, "project:ex/x", "  we use PNPM only "); err != nil || m.ID != rej.ID || m.Status != memory.StatusDeleted {
		t.Errorf("seen = %v %v, %v", m.ID, m.Status, err)
	}
	if _, err := s.SeenContent(ctx, "project:ex/other", "We use pnpm only."); !errors.Is(err, ErrNotFound) {
		t.Errorf("seen across scopes: %v", err)
	}

	// Edges and provenance.
	fact := mk(memory.TypeCodebase, "project:ex/x", "The login test raced on the session cookie.", memory.StatusProposed)
	if err := s.AddEdge(ctx, fact.ID, "derived_from", older.ID, "kenfold-extractor"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEdge(ctx, fact.ID, "derived_from", older.ID, "kenfold-extractor"); err != nil {
		t.Errorf("duplicate edge: %v", err)
	}
	if from, err := s.DerivedFrom(ctx, fact.ID); err != nil || len(from) != 1 || from[0].ID != older.ID {
		t.Errorf("derived from = %v, %v", contentsM(from), err)
	}
	if err := s.AddEdge(ctx, "bad", "derived_from", older.ID, "x"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("bad edge id: %v", err)
	}

	// SimilarAny also sees proposed memories; Similar does not.
	if got, _ := s.Similar(ctx, SimilarParams{Scope: "project:ex/x", Content: "The login test raced on the session cookie again."}); len(got) != 0 {
		t.Errorf("Similar returned proposed: %v", contents(got))
	}
	if got, err := s.SimilarAny(ctx, SimilarParams{Scope: "project:ex/x", Content: "The login test raced on the session cookie again."}); err != nil || len(got) != 1 || got[0].ID != fact.ID {
		t.Errorf("SimilarAny = %v, %v", contents(got), err)
	}

	// Reject: only proposed memories.
	if r, err := s.Reject(ctx, fact.ID, "not useful", "kenfold-cli"); err != nil || r.Status != memory.StatusDeleted || r.Attrs["forget_reason"] != "not useful" {
		t.Errorf("reject = %+v, %v", r.Attrs, err)
	}
	if _, err := s.Reject(ctx, older.ID, "x", "kenfold-cli"); !errors.Is(err, ErrNotProposed) {
		t.Errorf("reject active: %v", err)
	}

	// ApproveReplacing: the new memory wins, the old one becomes history.
	oldFact := mk(memory.TypeProject, "project:ex/x", "We deploy on Fridays.", memory.StatusActive)
	newFact := mk(memory.TypeProject, "project:ex/x", "We never deploy on Fridays.", memory.StatusProposed)
	other := mk(memory.TypeProject, "project:ex/other", "Other project fact.", memory.StatusActive)
	if _, err := s.ApproveReplacing(ctx, newFact.ID, other.ID); err == nil {
		t.Error("replacing across scopes accepted")
	}
	if _, err := s.ApproveReplacing(ctx, newFact.ID, newFact.ID); err == nil {
		t.Error("self-replacement accepted")
	}
	got, err := s.ApproveReplacing(ctx, newFact.ID, oldFact.ID)
	if err != nil || got.Status != memory.StatusActive || got.Trust != memory.TrustUser || got.Supersedes == nil || *got.Supersedes != oldFact.ID {
		t.Fatalf("approve replacing = %+v, %v", got, err)
	}
	if o, _ := s.Get(ctx, oldFact.ID); o.Status != memory.StatusSuperseded {
		t.Errorf("old fact = %s", o.Status)
	}
	if _, err := s.ApproveReplacing(ctx, newFact.ID, oldFact.ID); !errors.Is(err, ErrNotProposed) {
		t.Errorf("second approve replacing: %v", err)
	}
}
