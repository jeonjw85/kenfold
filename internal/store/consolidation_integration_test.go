package store

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/migrations"
)

// TestConsolidationStoreIntegration covers candidate pairs and digests,
// proposals (judged once), apply, stale proposals, and reject. It TRUNCATES
// memory; use a throwaway database.
func TestConsolidationStoreIntegration(t *testing.T) {
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
	axis := func(i int, tilt float32) []float32 {
		v := make([]float32, 1024)
		v[i], v[i+1] = 1, tilt
		return v
	}
	mk := func(typ memory.Type, scope, content string, vec []float32) Memory {
		t.Helper()
		p := CreateParams{Type: typ, Scope: scope, Content: content, SourceAgent: "codex", Trust: memory.TrustAgent, Confidence: 0.5, Status: memory.StatusActive}
		if vec != nil {
			p.Embedding, p.EmbeddingModel = vec, "m1"
		}
		m, err := s.Create(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct created_at
		return m
	}
	const scope = "project:ex/consol"
	a1 := mk(memory.TypeProject, scope, "Use pnpm, not npm, to install packages.", axis(0, 0))
	a2 := mk(memory.TypeProject, scope, "Install packages with pnpm; npm is not used here.", axis(0, 0.1))
	mk(memory.TypePreference, scope, "Use pnpm, not npm, to install packages.", axis(0, 0)) // another type
	far := mk(memory.TypeProject, scope, "The API listens on port 8080.", axis(10, 0))
	t1 := mk(memory.TypeCodebase, scope, "The webhook handler dedupes events by event_id in handler.go.", nil)
	t2 := mk(memory.TypeCodebase, scope, "The webhook handler dedupes events by event_id in handler.go now.", nil)
	mk(memory.TypeCodebase, "project:ex/other", "The webhook handler dedupes events by event_id in handler.go.", nil) // another scope
	mk(memory.TypeEpisodic, scope, "Session: used pnpm, not npm, to install packages.", axis(0, 0))                   // episodic
	var before time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	mk(memory.TypeProject, scope, "Packages are installed with pnpm, never npm.", axis(0, 0.05)) // not settled

	pairs, err := s.ConsolidationPairs(ctx, PairParams{Before: before})
	if err != nil {
		t.Fatal(err)
	}
	got := map[[2]string]bool{}
	for _, p := range pairs {
		if !p.A.CreatedAt.Before(p.B.CreatedAt) {
			t.Errorf("pair %s/%s: A is not the older", p.A.ID, p.B.ID)
		}
		got[[2]string{p.A.ID, p.B.ID}] = true
	}
	if len(pairs) != 2 || !got[[2]string{a1.ID, a2.ID}] || !got[[2]string{t1.ID, t2.ID}] {
		for _, p := range pairs {
			t.Logf("pair %q / %q (%.2f)", p.A.Content, p.B.Content, p.Score)
		}
		t.Fatalf("pairs = %d, want the vector pair and the trigram pair", len(pairs))
	}

	// A member set is judged once; dismissed pairs are not offered again.
	dup, ok, err := s.CreateProposal(ctx, NewProposal{Kind: KindDuplicate, Scope: scope, MemberIDs: []string{a2.ID, a1.ID}, Keep: a2.ID, Reason: "same rule", Model: "m"})
	if err != nil || !ok || dup.Status != ProposalPending || dup.MemberIDs[0] > dup.MemberIDs[1] {
		t.Fatalf("create proposal = %+v %v %v", dup, ok, err)
	}
	if _, ok, err := s.CreateProposal(ctx, NewProposal{Kind: KindConflict, Scope: scope, MemberIDs: []string{a1.ID, a2.ID}, Keep: a1.ID, Model: "m"}); ok || err != nil {
		t.Errorf("second proposal for the same pair: %v %v", ok, err)
	}
	if d, ok, err := s.CreateProposal(ctx, NewProposal{Kind: KindDistinct, Scope: scope, MemberIDs: []string{t1.ID, t2.ID}, Model: "m"}); !ok || err != nil || d.Status != ProposalDismissed {
		t.Errorf("distinct pair = %+v %v %v", d, ok, err)
	}
	if pairs, err := s.ConsolidationPairs(ctx, PairParams{Before: before}); err != nil || len(pairs) != 0 {
		t.Errorf("pairs after judging = %d, %v", len(pairs), err)
	}
	if _, _, err := s.CreateProposal(ctx, NewProposal{Kind: KindDuplicate, Scope: scope, MemberIDs: []string{a1.ID}, Model: "m"}); err == nil {
		t.Error("one-member proposal accepted")
	}

	ps, err := s.PendingProposals(ctx, 10)
	if err != nil || len(ps) != 1 || len(ps[0].Members) != 2 || ps[0].Members[0].ID != a1.ID {
		t.Fatalf("pending = %+v, %v", ps, err)
	}
	if n, err := s.CountPendingProposals(ctx); n != 1 || err != nil {
		t.Errorf("pending count = %d, %v", n, err)
	}

	// Apply a duplicate: the kept memory stays, the other becomes history.
	applied, err := s.ApplyProposal(ctx, dup.ID, "kenfold-dashboard")
	if err != nil || applied.Status != ProposalApplied || applied.ResultID != a2.ID || applied.DecidedBy != "kenfold-dashboard" {
		t.Fatalf("apply = %+v, %v", applied, err)
	}
	if m, _ := s.Get(ctx, a1.ID); m.Status != memory.StatusSuperseded {
		t.Errorf("retired duplicate status = %s", m.Status)
	}
	if m, _ := s.Get(ctx, a2.ID); m.Status != memory.StatusActive {
		t.Errorf("kept memory status = %s", m.Status)
	}
	links, _ := s.Links(ctx, a2.ID)
	if len(links) != 1 || links[0].Relation != RelReplaces || !links[0].Outgoing || links[0].Memory.ID != a1.ID {
		t.Errorf("links of the kept memory = %+v", links)
	}
	if _, err := s.ApplyProposal(ctx, dup.ID, "kenfold-dashboard"); !errors.Is(err, ErrProposalDecided) {
		t.Errorf("second apply: %v", err)
	}

	// A proposal whose member was forgotten is stale and cannot be applied.
	conflict, _, err := s.CreateProposal(ctx, NewProposal{Kind: KindConflict, Scope: scope, MemberIDs: []string{far.ID, a2.ID}, Keep: a2.ID, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SoftDelete(ctx, far.ID, "outdated", "t"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountPendingProposals(ctx); n != 0 {
		t.Errorf("a stale proposal is counted as pending")
	}
	if _, err := s.ApplyProposal(ctx, conflict.ID, "t"); !errors.Is(err, ErrProposalStale) {
		t.Errorf("apply stale: %v", err)
	}
	var st string
	if err := pool.QueryRow(ctx, `SELECT status FROM consolidation WHERE id = $1`, conflict.ID).Scan(&st); err != nil || st != ProposalStale {
		t.Errorf("stale proposal status = %q, %v", st, err)
	}
	if m, _ := s.Get(ctx, a2.ID); m.Status != memory.StatusActive {
		t.Errorf("a stale apply changed a memory: %s", m.Status)
	}

	// Reject: kept as they are; deciding twice is an error.
	r1 := mk(memory.TypeSemantic, scope, "Postgres 18 is required.", nil)
	r2 := mk(memory.TypeSemantic, scope, "PostgreSQL 18 or newer is needed.", nil)
	rej, _, err := s.CreateProposal(ctx, NewProposal{Kind: KindDuplicate, Scope: scope, MemberIDs: []string{r1.ID, r2.ID}, Keep: r1.ID, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RejectProposal(ctx, rej.ID, "t"); err != nil {
		t.Errorf("reject: %v", err)
	}
	if err := s.RejectProposal(ctx, rej.ID, "t"); !errors.Is(err, ErrProposalDecided) {
		t.Errorf("second reject: %v", err)
	}
	if err := s.RejectProposal(ctx, "01a0e2f4-5446-7fe9-bb7a-89d49ad44bfa", "t"); !errors.Is(err, ErrNotFound) {
		t.Errorf("reject unknown: %v", err)
	}
	if m, _ := s.Get(ctx, r2.ID); m.Status != memory.StatusActive {
		t.Errorf("reject changed a memory: %s", m.Status)
	}

	// Digests: the oldest old sessions of a scope, then a dated digest.
	const dscope = "project:ex/digest"
	var sessions []Memory
	for i := range 7 {
		sessions = append(sessions, mk(memory.TypeEpisodic, dscope, "Session summary "+strconv.Itoa(i)+": worked on the importer.", nil))
	}
	cutoff := time.Now()
	groups, err := s.DigestGroups(ctx, DigestParams{Before: cutoff, Min: 6, Max: 5})
	if err != nil || len(groups) != 1 || groups[0].Scope != dscope || len(groups[0].Sessions) != 5 || groups[0].Sessions[0].ID != sessions[0].ID {
		t.Fatalf("digest groups = %+v, %v", groups, err)
	}
	var ids []string
	for _, m := range groups[0].Sessions {
		ids = append(ids, m.ID)
	}
	dig, ok, err := s.CreateProposal(ctx, NewProposal{Kind: KindDigest, Scope: dscope, MemberIDs: ids, Content: "Worked on the importer across five sessions.", Model: "m"})
	if err != nil || !ok {
		t.Fatalf("digest proposal: %v %v", ok, err)
	}
	if groups, _ := s.DigestGroups(ctx, DigestParams{Before: cutoff, Min: 2, Max: 5}); len(groups) != 1 || len(groups[0].Sessions) != 2 {
		t.Errorf("sessions in a pending digest were offered again: %+v", groups)
	}
	applied, err = s.ApplyProposal(ctx, dig.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Get(ctx, applied.ResultID)
	if err != nil || d.Type != memory.TypeEpisodic || d.Status != memory.StatusActive || d.SourceAgent != DigestAgent || d.Attrs["kind"] != "digest" ||
		!d.CreatedAt.Equal(sessions[4].CreatedAt) || d.Attrs["session_count"] != float64(5) {
		t.Fatalf("digest = %+v, %v", d, err)
	}
	for _, id := range ids {
		if m, _ := s.Get(ctx, id); m.Status != memory.StatusSuperseded {
			t.Errorf("digested session status = %s", m.Status)
		}
	}
	if links, _ := s.Links(ctx, d.ID); len(links) != 5 {
		t.Errorf("digest links = %d", len(links))
	}
	// The extractor skips digests (their sessions were extracted), also
	// imported ones, and does not count them as waiting.
	if st, err := s.ExtractionStatus(ctx); err != nil || st.Pending != 3 {
		t.Errorf("extraction pending = %d, %v; want the 3 undigested sessions", st.Pending, err)
	}
	for range 5 {
		job, err := s.ClaimExtraction(ctx, time.Now(), time.Minute, "m")
		if errors.Is(err, ErrNotFound) {
			break
		}
		if err != nil || job.Source.ID == d.ID {
			t.Fatalf("claim = %s, %v; the digest was claimed for extraction", job.Source.ID, err)
		}
	}
	if rec, _ := s.RecentEpisodes(ctx, dscope, 3); len(rec) == 0 || rec[0].ID == d.ID {
		t.Errorf("the digest counts as the most recent session")
	}
	// A digest is not digested again; the two remaining sessions are too few.
	if groups, _ := s.DigestGroups(ctx, DigestParams{Before: time.Now(), Min: 3}); len(groups) != 0 {
		t.Errorf("digest offered for digesting: %+v", groups)
	}

	counts, err := s.ProposalCounts(ctx)
	if err != nil || counts[ProposalApplied] != 2 || counts[ProposalStale] != 1 || counts[ProposalRejected] != 1 || counts[ProposalDismissed] != 1 {
		t.Errorf("counts = %v, %v", counts, err)
	}
}
