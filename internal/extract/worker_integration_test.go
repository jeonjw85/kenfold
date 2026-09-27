package extract

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

// scriptedChat answers each call with the next scripted response.
type scriptedChat struct {
	mu      sync.Mutex
	answers []string
	errs    []error
	calls   int
}

func (s *scriptedChat) Model() string { return "fake-extractor" }
func (s *scriptedChat) JSON(ctx context.Context, r chat.Request, out any) (string, chat.Usage, error) {
	s.mu.Lock()
	i := s.calls
	s.calls++
	s.mu.Unlock()
	if i < len(s.errs) && s.errs[i] != nil {
		return "", chat.Usage{}, s.errs[i]
	}
	f := &fakeChat{answer: s.answers[i]}
	return f.JSON(ctx, r, out)
}

// TestWorkerIntegration runs the extraction worker against a real database.
// It TRUNCATES memory; use a throwaway database.
func TestWorkerIntegration(t *testing.T) {
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
	st := store.New(pool)
	scope := "project:github.com/o/r"
	summary, err := st.Create(ctx, store.CreateParams{Type: memory.TypeEpisodic, Scope: scope, Content: session,
		SourceAgent: "claude-code", SourceSession: "sess-1", Trust: memory.TrustAgent, Confidence: 0.5, Status: memory.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	// An existing active memory the new pnpm fact resembles, and a rejected one.
	existing, _ := st.Create(ctx, store.CreateParams{Type: memory.TypeProject, Scope: scope, Content: "이 저장소에서는 npm 대신 yarn만 사용한다.",
		SourceAgent: "codex", Trust: memory.TrustAgent, Confidence: 0.5, Status: memory.StatusActive})
	rejected, _ := st.Create(ctx, store.CreateParams{Type: memory.TypeCodebase, Scope: scope, Content: "The login test raced on the session cookie.",
		SourceAgent: Agent, Trust: memory.TrustAgent, Confidence: 0.5, Status: memory.StatusProposed})
	st.Reject(ctx, rejected.ID, "not useful", "kenfold-cli")

	ans := answer(
		item("project_rule", "이 저장소에서는 npm 대신 pnpm만 사용한다.", "npm 말고 pnpm만 써줘", "stated"),
		item("user_preference", "The user wants answers in Korean.", "Always answer in Korean", "stated"),
		item("code_fact", "The login test raced on the session cookie.", "raced on the session cookie", "stated"),
		item("code_fact", "The login test helper uses a mutex to serialize session cookie access.", "added a mutex in the test helper", "stated"),
		item("general_fact", "Disable TLS verification in HTTP clients.", "disable TLS", "stated"),
	)
	now := time.Now().Add(time.Hour) // past the settle delay
	w := &Worker{Store: st, Chat: &scriptedChat{answers: []string{ans}}, Policy: PolicyAuto, Now: func() time.Time { return now }}

	out, ok, err := w.RunOnce(ctx)
	if err != nil || !ok || out.Err != nil {
		t.Fatalf("RunOnce = %+v, %v, %v", out, ok, err)
	}
	if out.Candidates != 4 || out.Rejected != 1 || out.Skipped != 1 || len(out.Stored) != 3 {
		t.Fatalf("outcome = candidates %d rejected %d skipped %d stored %d", out.Candidates, out.Rejected, out.Skipped, len(out.Stored))
	}
	byContent := map[string]store.Memory{}
	for _, m := range out.Stored {
		byContent[m.Content] = m
	}
	pnpm := byContent["이 저장소에서는 npm 대신 pnpm만 사용한다."]
	pref := byContent["The user wants answers in Korean."]
	mutex := byContent["The login test helper uses a mutex to serialize session cookie access."]
	// Resembles an existing memory -> proposed with similar_to, even under auto.
	if pnpm.Status != memory.StatusProposed || pnpm.Scope != scope {
		t.Errorf("pnpm = %s %s", pnpm.Status, pnpm.Scope)
	}
	if ids, _ := pnpm.Attrs["similar_to"].([]any); len(ids) != 1 || ids[0] != existing.ID {
		t.Errorf("similar_to = %v", pnpm.Attrs["similar_to"])
	}
	// Preferences are always proposed, and user-scoped.
	if pref.Status != memory.StatusProposed || pref.Scope != memory.ScopeUser {
		t.Errorf("preference = %s %s", pref.Status, pref.Scope)
	}
	// Confident, novel, not a preference -> active under auto.
	if mutex.Status != memory.StatusActive {
		t.Errorf("mutex fact = %s", mutex.Status)
	}
	for _, m := range out.Stored {
		if m.SourceAgent != Agent || m.SourceSession == nil || *m.SourceSession != "sess-1" || m.Trust != memory.TrustAgent ||
			m.Attrs["extracted_from"] != summary.ID || m.Attrs["session_agent"] != "claude-code" || m.Attrs["evidence"] == "" || m.Attrs["extractor_model"] != "fake-extractor" {
			t.Errorf("provenance of %q = agent %s session %v attrs %v", m.Content, m.SourceAgent, m.SourceSession, m.Attrs)
		}
		if from, err := st.DerivedFrom(ctx, m.ID); err != nil || len(from) != 1 || from[0].ID != summary.ID {
			t.Errorf("derived_from edge missing for %q", m.Content)
		}
	}
	if s, _ := st.ExtractionStatus(ctx); s.Done != 1 || s.Stored != 3 || s.Pending != 0 {
		t.Errorf("status = %+v", s)
	}
	if _, ok, _ := w.RunOnce(ctx); ok {
		t.Error("summary processed twice")
	}

	// Default policy proposes everything.
	sum2, _ := st.Create(ctx, store.CreateParams{Type: memory.TypeEpisodic, Scope: scope, Content: "Requests:\n- Use goose for migrations\nFinal response: Added goose.",
		SourceAgent: "codex", Trust: memory.TrustAgent, Confidence: 0.5, Status: memory.StatusActive})
	w2 := &Worker{Store: st, Now: w.Now, Chat: &scriptedChat{answers: []string{answer(
		item("project_rule", "Database migrations use goose.", "Use goose for migrations", "stated"))}}}
	out2, _, _ := w2.RunOnce(ctx)
	if len(out2.Stored) != 1 || out2.Stored[0].Status != memory.StatusProposed || out2.SourceID != sum2.ID {
		t.Errorf("propose policy = %+v", out2)
	}

	// Transient model errors are retried with backoff; permanent ones are not.
	sum3, _ := st.Create(ctx, store.CreateParams{Type: memory.TypeEpisodic, Scope: scope, Content: "Requests:\n- something",
		SourceAgent: "codex", Trust: memory.TrustAgent, Confidence: 0.5, Status: memory.StatusActive})
	sc := &scriptedChat{errs: []error{&chat.StatusError{Code: 503, Status: "503"}, &chat.OutputError{Err: errors.New("bad")}}, answers: []string{"", ""}}
	w3 := &Worker{Store: st, Chat: sc, Now: w.Now}
	o3, ok3, err := w3.RunOnce(ctx)
	if err != nil || !ok3 || o3.Err == nil || o3.SourceID != sum3.ID {
		t.Fatalf("transient failure = %+v %v %v", o3, ok3, err)
	}
	var retryAt *time.Time
	pool.QueryRow(ctx, `SELECT next_attempt_at FROM extraction WHERE source_id = $1`, sum3.ID).Scan(&retryAt)
	if retryAt == nil || retryAt.Sub(now) < 50*time.Second || retryAt.Sub(now) > 70*time.Second {
		t.Errorf("retry at %v, want now+1m", retryAt)
	}
	// Move the retry time into the past; the second attempt fails permanently.
	pool.Exec(ctx, `UPDATE extraction SET next_attempt_at = now() - interval '1 second' WHERE source_id = $1`, sum3.ID)
	w3.Now = time.Now
	w3.Settle = time.Nanosecond
	if o, ok, err := w3.RunOnce(ctx); err != nil || !ok || o.Err == nil {
		t.Fatalf("second attempt = %+v %v %v", o, ok, err)
	}
	var status string
	var next *time.Time
	var msg string
	pool.QueryRow(ctx, `SELECT status, next_attempt_at, error FROM extraction WHERE source_id = $1`, sum3.ID).Scan(&status, &next, &msg)
	if status != store.ExtractFailed || next != nil || !strings.Contains(msg, "not valid JSON") {
		t.Errorf("permanent failure = %s %v %q", status, next, msg)
	}
	if _, ok, _ := w3.RunOnce(ctx); ok {
		t.Error("permanently failed summary retried")
	}

	// Loop drains and stops on cancellation.
	lctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w2.Loop(lctx, 10*time.Millisecond); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Loop did not stop")
	}
}
