package bench

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
)

func retryTestKey(c *ScoreCache) scoreKey {
	return scoreKey{Version: 1, IdentitySHA256: c.identity, Scope: "project:retry", ID: "q1",
		Text: "question", Gold: "gold", ReaderModel: "reader", JudgeModel: "judge", ReaderTokens: 8, JudgeTokens: 8, TopK: 10}
}

func retryTestClient(t *testing.T, status func(call int) int) *chat.Client {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := status(int(calls.Add(1))); code != 0 {
			http.Error(w, "try later", code)
			return
		}
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`)
	}))
	t.Cleanup(srv.Close)
	c, err := chat.New(chat.Config{BaseURL: srv.URL, Model: "reader", Reasoning: "none"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestScoreCacheRetriesUnbilled verifies that a rate-limited or failed call is
// retried instead of poisoning the checkpoint: no completion was produced, so
// nothing was billed and the question's outcome is still unknown only in the
// sense that it must be asked again.
func TestScoreCacheRetriesUnbilled(t *testing.T) {
	c, err := NewScoreCache(filepath.Join(t.TempDir(), "scores"), "retry-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.retryDelay = func(int) time.Duration { return 0 }
	client := retryTestClient(t, func(call int) int {
		if call <= 2 {
			return http.StatusTooManyRequests
		}
		return 0
	})
	b, err := NewBudget(1, 1.25, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	key := retryTestKey(c)
	state := scoreState{Key: key}
	text, err := c.complete(context.Background(), &state, scoreReaderInFlight, client, "", "hello", 8, b)
	if err != nil || text != "ok" {
		t.Fatalf("complete = %q, %v", text, err)
	}
	if b.calls != 1 || b.reserved != 0 || math.Abs(b.SpentUSD()-0.0015) > 1e-12 || b.stopped != nil {
		t.Fatalf("budget after retries: calls=%d reserved=%g spent=%g stop=%v", b.calls, b.reserved, b.SpentUSD(), b.stopped)
	}
	got, hit, err := c.load(key)
	if err != nil || !hit || got.Phase != scoreReaderInFlight {
		t.Fatalf("checkpoint = %+v, hit=%v, err=%v", got, hit, err)
	}
}

// TestScoreCacheClearsMarkerAfterPersistentFailure verifies that a question
// whose calls keep failing is left retryable: the in-flight marker is removed
// so the next run does not refuse to start.
func TestScoreCacheClearsMarkerAfterPersistentFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scores")
	c, err := NewScoreCache(dir, "retry-v1")
	if err != nil {
		t.Fatal(err)
	}
	c.retryDelay = func(int) time.Duration { return 0 }
	client := retryTestClient(t, func(int) int { return http.StatusServiceUnavailable })
	b, err := NewBudget(1, 1.25, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	key := retryTestKey(c)
	state := scoreState{Key: key}
	if _, err := c.complete(context.Background(), &state, scoreReaderInFlight, client, "", "hello", 8, b); !errors.Is(err, ErrScoreCallUnknown) {
		t.Fatalf("persistent failure = %v, want ErrScoreCallUnknown", err)
	}
	if b.stopped != nil || b.calls != 0 || b.reserved != 0 || b.SpentUSD() != 0 {
		t.Fatalf("unbilled failure changed accounting: stop=%v calls=%d reserved=%g spent=%g", b.stopped, b.calls, b.reserved, b.SpentUSD())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewScoreCache(dir, "retry-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, hit, err := restarted.load(key); err != nil || hit {
		t.Fatalf("in-flight marker survived: hit=%v err=%v", hit, err)
	}
}
