package bench

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"

	"github.com/kenfold/kenfold/internal/chat"
)

var (
	ErrBudgetExceeded         = errors.New("benchmark API budget would be exceeded")
	ErrBudgetUsageUnavailable = errors.New("benchmark API usage unavailable; stopping to protect budget")
)

// Budget accounts for reader/judge calls only. Prices must cover every model
// and context tier used by the caller. Reasoning and paid tools must be disabled.
// Cache discounts are ignored, so reported cost is an upper estimate, not a bill.
type Budget struct {
	mu                        sync.Mutex
	limit, input, output      float64
	spent, reserved           float64
	calls, prompt, completion int
	stopped                   error
	journal                   *budgetJournal
	pending                   int
}

func NewBudget(limit, inputUSDPerMillion, outputUSDPerMillion float64) (*Budget, error) {
	for _, v := range []float64{limit, inputUSDPerMillion, outputUSDPerMillion} {
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, errors.New("benchmark budget and token prices must be finite and positive")
		}
	}
	return &Budget{limit: limit, input: inputUSDPerMillion, output: outputUSDPerMillion}, nil
}

func (b *Budget) reserve(system, user string, maxTokens int) (float64, error) {
	if b == nil {
		return 0, nil
	}
	if maxTokens <= 0 {
		return 0, errors.New("budgeted completions require a positive output token limit")
	}
	// At most one text token per UTF-8 byte, plus a generous template allowance.
	// Reserve at least $0.05 for an unaccounted/failed request's possible charge.
	amount := math.Max(0.05, (float64(len(system))+float64(len(user))+1024)*b.input/1e6+float64(maxTokens)*b.output/1e6)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped != nil {
		return 0, b.stopped
	}
	if b.journal != nil && b.journal.lock == nil {
		return 0, b.journalFailure(errors.New("journal is closed"))
	}
	if b.spent+b.reserved+amount > b.limit {
		return 0, ErrBudgetExceeded
	}
	b.reserved += amount
	if b.journal != nil {
		b.pending++
		if err := b.persistJournal(); err != nil {
			return 0, b.journalFailure(err)
		}
	}
	return amount, nil
}

func (b *Budget) settle(reserved float64, usage chat.Usage) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.journal != nil {
		if b.journal.lock == nil || b.pending <= 0 || reserved <= 0 || math.IsNaN(reserved) || math.IsInf(reserved, 0) || reserved > b.reserved+1e-12 {
			return b.journalFailure(errors.New("invalid journaled settlement or closed journal"))
		}
		b.pending--
	}
	b.reserved -= reserved
	if b.journal != nil && b.pending == 0 {
		b.reserved = 0
	}
	b.calls++
	// These answer/judge completions require nonempty output. A zero count is
	// also how the chat decoder represents missing/null fields: fail closed.
	if usage.PromptTokens <= 0 || usage.CompletionTokens <= 0 {
		b.spent += reserved
		b.stopped = ErrBudgetUsageUnavailable
		if err := b.persistJournal(); err != nil {
			return b.journalFailure(err)
		}
		return b.stopped
	}
	cost := float64(usage.PromptTokens)*b.input/1e6 + float64(usage.CompletionTokens)*b.output/1e6
	b.spent += cost
	b.prompt += usage.PromptTokens
	b.completion += usage.CompletionTokens
	// Unknown usage includes a conservative charge not represented by tokens.
	// Keep that stop while settling other already-reserved responses durably.
	if cost > reserved && !errors.Is(b.stopped, ErrBudgetUsageUnavailable) {
		b.stopped = fmt.Errorf("%w: response usage exceeded its conservative reservation", ErrBudgetExceeded)
	}
	if err := b.persistJournal(); err != nil {
		return b.journalFailure(err)
	}
	if cost > reserved || b.journal != nil {
		return b.stopped
	}
	return nil
}

func (b *Budget) SpentUSD() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

func (b *Budget) Note() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Sprintf("External API cost upper estimate: $%.6f of $%.2f; %d calls, %d input and %d output tokens. Cache discounts ignored; missing usage retains the full reservation and stops further calls.", b.spent, b.limit, b.calls, b.prompt, b.completion)
}

func budgetFromEnv() (*Budget, error) {
	if os.Getenv("KENFOLD_BENCH_BUDGET_USD") == "" {
		if os.Getenv("KENFOLD_BENCH_INPUT_USD_PER_M") != "" || os.Getenv("KENFOLD_BENCH_OUTPUT_USD_PER_M") != "" {
			return nil, errors.New("benchmark token prices require KENFOLD_BENCH_BUDGET_USD")
		}
		return nil, nil
	}
	if os.Getenv("KENFOLD_BENCH_REASONING") != "none" {
		return nil, errors.New("budgeted benchmarks require KENFOLD_BENCH_REASONING=none")
	}
	var values []float64
	for _, name := range []string{"KENFOLD_BENCH_BUDGET_USD", "KENFOLD_BENCH_INPUT_USD_PER_M", "KENFOLD_BENCH_OUTPUT_USD_PER_M"} {
		v, err := strconv.ParseFloat(os.Getenv(name), 64)
		if err != nil {
			return nil, fmt.Errorf("%s must contain a finite positive number", name)
		}
		values = append(values, v)
	}
	return NewBudget(values[0], values[1], values[2])
}
