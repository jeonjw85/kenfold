package bench

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/store"
	"github.com/kenfold/kenfold/migrations"
)

func TestBudgetRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		limit, input, output float64
	}{
		{"zero limit", 0, 1.25, 2.5},
		{"negative limit", -1, 1.25, 2.5},
		{"nan limit", math.NaN(), 1.25, 2.5},
		{"infinite limit", math.Inf(1), 1.25, 2.5},
		{"zero input price", 1, 0, 2.5},
		{"negative output price", 1, 1.25, -1},
		{"nan input price", 1, math.NaN(), 2.5},
		{"infinite output price", 1, 1.25, math.Inf(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewBudget(tc.limit, tc.input, tc.output); err == nil {
				t.Fatal("invalid budget configuration accepted")
			}
		})
	}
}

func TestBudgetBlocksRequestBeforeNetworkCall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":100,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	c, err := chat.New(chat.Config{BaseURL: srv.URL, Model: "test", Reasoning: "none"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBudget(0.001, 1.25, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	_, err = completeText(context.Background(), c, "", "hello", 8, b)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("want budget rejection, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("rejected request reached server %d times", calls.Load())
	}
}

func TestBudgetAccountsUsageAndReleasesReservation(t *testing.T) {
	b, err := NewBudget(0.1, 1.25, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reserved, err := b.reserve("", "hello", 8)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.settle(reserved, chat.Usage{PromptTokens: 1000, CompletionTokens: 100}); err != nil {
			t.Fatal(err)
		}
	}
	// Each reply costs 1000 * $1.25/M + 100 * $2.50/M = $0.0015.
	if got := b.SpentUSD(); math.Abs(got-0.003) > 1e-12 {
		t.Fatalf("spent = %.9f, want 0.003", got)
	}
	if _, err := b.reserve("", "hello", 8); err != nil {
		t.Fatalf("completed reservations were not released: %v", err)
	}
}

func TestBudgetReservesConcurrentRequestsAtomically(t *testing.T) {
	b, err := NewBudget(0.05, 1.25, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.reserve("", "hello", 8); err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, ErrBudgetExceeded) {
				t.Errorf("unexpected reserve error: %v", err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d requests against a one-request budget", accepted.Load())
	}
}

func TestBudgetMissingUsageStopsFurtherBillableCalls(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	c, err := chat.New(chat.Config{BaseURL: srv.URL, Model: "test", Reasoning: "none"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBudget(1, 1.25, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := completeText(context.Background(), c, "", "hello", 8, b); !errors.Is(err, ErrBudgetUsageUnavailable) {
			t.Fatalf("want fail-closed accounting error, got %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("missing usage caused %d billable requests", calls.Load())
	}
	if b.SpentUSD() < 0.05 {
		t.Fatalf("uncertain request was not charged at its reserved maximum: %f", b.SpentUSD())
	}
}

func TestBudgetIncompleteUsageStopsFurtherBillableCalls(t *testing.T) {
	for _, tc := range []struct{ name, usage string }{
		{"missing output", `{"prompt_tokens":1000}`},
		{"null output", `{"prompt_tokens":1000,"completion_tokens":null}`},
		{"zero output", `{"prompt_tokens":1000,"completion_tokens":0}`},
		{"null input", `{"prompt_tokens":null,"completion_tokens":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":%s}`, tc.usage)
			}))
			defer srv.Close()
			c, err := chat.New(chat.Config{BaseURL: srv.URL, Model: "test", Reasoning: "none"})
			if err != nil {
				t.Fatal(err)
			}
			b, err := NewBudget(1, 1.25, 2.5)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := completeText(context.Background(), c, "", "hello", 8, b); !errors.Is(err, ErrBudgetUsageUnavailable) {
					t.Fatalf("incomplete usage did not fail closed: %v", err)
				}
			}
			if calls.Load() != 1 || b.SpentUSD() < 0.05 {
				t.Fatalf("uncertain usage retried or discarded reservation: calls=%d, cost=%f", calls.Load(), b.SpentUSD())
			}
		})
	}
}

func TestBudgetHTTPFailureDoesNotRetryWithoutUsage(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c, err := chat.New(chat.Config{BaseURL: srv.URL, Model: "test", Reasoning: "none"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBudget(1, 1.25, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completeText(context.Background(), c, "", "hello", 8, b); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("want fail-closed accounting error, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("failed request was retried %d times", calls.Load())
	}
}

func TestBudgetRejectsUnboundedOutput(t *testing.T) {
	b, err := NewBudget(1, 1.25, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	for _, maxTokens := range []int{0, -1} {
		if _, err := b.reserve("", "hello", maxTokens); err == nil {
			t.Fatalf("accepted unbounded max_tokens %d", maxTokens)
		}
	}
}

func TestBudgetIncludesMultibytePromptAndOutputLimit(t *testing.T) {
	b, err := NewBudget(0.1, 1.25, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.reserve("", strings.Repeat("한", 40000), 8); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("multibyte prompt escaped budget: %v", err)
	}
	if _, err := b.reserve("", "hello", 100000); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("output limit escaped budget: %v", err)
	}
}

func TestBudgetStopPreservesScoredRowsIntegration(t *testing.T) {
	dbURL := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"yes"}}],"usage":{"prompt_tokens":20000,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	c, err := chat.New(chat.Config{BaseURL: srv.URL, Model: "test", Reasoning: "none"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBudget(0.09, 1.25, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	qs := []scoredQ{
		{Question: Question{ID: "q1", Type: "1", Text: "first question", Answer: "yes"}, Scope: "budget-test"},
		{Question: Question{ID: "q2", Type: "1", Text: "second question", Answer: "yes"}, Scope: "budget-test"},
	}
	rows, err := scoreQuestions(ctx, Deps{Store: store.New(pool), Reader: c, Judge: c, Budget: b}, qs, true, t.Logf)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("budget stop must propagate, got %v", err)
	}
	if len(rows) != 2 || rows[0].Label != "all" || rows[0].N != 1 || rows[0].Judge != 1 || rows[0].Errors != 0 {
		t.Fatalf("completed scores lost or an unasked question counted: %+v", rows)
	}
	if calls.Load() != 2 {
		t.Fatalf("budget stop made extra calls: %d", calls.Load())
	}
}

func TestBudgetConfigurationRequiresBoundedNonReasoningCalls(t *testing.T) {
	for _, tc := range []struct {
		name, limit, input, output, reasoning string
		wantErr, wantBudget                   bool
	}{
		{name: "disabled"},
		{"valid", "4.5", "1.25", "2.5", "none", false, true},
		{"invalid limit", "oops", "1.25", "2.5", "none", true, false},
		{"missing input price", "4.5", "", "2.5", "none", true, false},
		{"zero output price", "4.5", "1.25", "0", "none", true, false},
		{"reasoning omitted", "4.5", "1.25", "2.5", "", true, false},
		{"reasoning enabled", "4.5", "1.25", "2.5", "low", true, false},
		{"prices without limit", "", "1.25", "2.5", "none", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KENFOLD_BENCH_BUDGET_USD", tc.limit)
			t.Setenv("KENFOLD_BENCH_INPUT_USD_PER_M", tc.input)
			t.Setenv("KENFOLD_BENCH_OUTPUT_USD_PER_M", tc.output)
			t.Setenv("KENFOLD_BENCH_REASONING", tc.reasoning)
			b, err := budgetFromEnv()
			if (err != nil) != tc.wantErr || (b != nil) != tc.wantBudget {
				t.Fatalf("budget present = %v, err = %v", b != nil, err)
			}
		})
	}
}

func TestBudgetDisabledAllowsProviderWithoutUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	c, err := chat.New(chat.Config{BaseURL: srv.URL, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	text, err := completeText(context.Background(), c, "", "hello", 8, nil)
	if err != nil || text != "ok" {
		t.Fatalf("unbudgeted completion changed: %q, %v", text, err)
	}
}

func TestBudgetTruncatedAnswerStillAccountsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"finish_reason":"length","message":{"content":"partial"}}],"usage":{"prompt_tokens":1000,"completion_tokens":256}}`)
	}))
	defer srv.Close()
	c, err := chat.New(chat.Config{BaseURL: srv.URL, Model: "test", Reasoning: "none"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBudget(1, 1.25, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completeText(context.Background(), c, "", "hello", 256, b); !errors.Is(err, chat.ErrTruncated) {
		t.Fatalf("want truncated output, got %v", err)
	}
	if math.Abs(b.SpentUSD()-0.00189) > 1e-12 {
		t.Fatalf("truncated output cost not accounted: %.9f", b.SpentUSD())
	}
}

func TestBudgetUnderestimatedReservationFailsClosed(t *testing.T) {
	b, err := NewBudget(1, 1.25, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := b.reserve("", "hello", 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.settle(reserved, chat.Usage{PromptTokens: 100000, CompletionTokens: 1}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("underestimated usage did not stop: %v", err)
	}
	if _, err := b.reserve("", "hello", 8); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("underestimated response permitted more calls: %v", err)
	}
	if math.Abs(b.SpentUSD()-0.1250025) > 1e-12 {
		t.Fatalf("actual usage was not recorded: %f", b.SpentUSD())
	}
}
