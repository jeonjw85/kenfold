package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
)

func TestBudgetJournalSettledUsageSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := journalBudgetForTest(t, 0.1, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	reserved, err := b.reserve("", "hello", 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.settle(reserved, chat.Usage{PromptTokens: 1000, CompletionTokens: 100}); err != nil {
		t.Fatal(err)
	}
	if err := b.closeJournal(); err != nil {
		t.Fatal(err)
	}
	b = journalBudgetForTest(t, 0.1, 1.25, 2.5)
	if err := b.enableJournal(path); err != nil {
		t.Fatal(err)
	}
	defer b.closeJournal()
	if math.Abs(b.SpentUSD()-0.0015) > 1e-12 || b.calls != 1 || b.prompt != 1000 || b.completion != 100 || b.reserved != 0 {
		t.Fatalf("restart discarded settled usage: spent=%g calls=%d tokens=%d/%d reserved=%g", b.SpentUSD(), b.calls, b.prompt, b.completion, b.reserved)
	}
}

func TestBudgetJournalPendingReservationBlocksRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	if _, err := b.reserve("", "hello", 8); err != nil {
		t.Fatal(err)
	}
	if err := b.closeJournal(); err != nil {
		t.Fatal(err)
	}
	b = journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.enableJournal(path); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("uncertain in-flight request accepted on restart: %v", err)
	}
	if _, err := b.reserve("", "hello", 8); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("restart allowed another paid request: %v", err)
	}
}

func TestBudgetJournalConcurrentWriterRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	defer b.closeJournal()
	other := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := other.enableJournal(path); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("second writer acquired the same allowance: %v", err)
	}
	if _, err := other.reserve("", "hello", 8); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("failed journal open did not fail closed: %v", err)
	}
}

func TestBudgetJournalMissingOrCorruptPreventsCalls(t *testing.T) {
	for _, body := range []string{"", "{", "null", `{}`, `{"version":999}`} {
		t.Run(fmt.Sprintf("body_%q", body), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "budget.json")
			if body != "" {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			b := journalBudgetForTest(t, 1, 1.25, 2.5)
			if err := b.enableJournal(path); !errors.Is(err, ErrBudgetUsageUnavailable) {
				t.Fatalf("missing/corrupt journal accepted: %v", err)
			}
			if _, err := b.reserve("", "hello", 8); !errors.Is(err, ErrBudgetUsageUnavailable) {
				t.Fatalf("failed journal open allowed request: %v", err)
			}
		})
	}
}

func TestBudgetJournalConfigurationChangeRejected(t *testing.T) {
	for _, cfg := range [][3]float64{{2, 1.25, 2.5}, {1, 2, 2.5}, {1, 1.25, 3}} {
		path := filepath.Join(t.TempDir(), "budget.json")
		b := journalBudgetForTest(t, 1, 1.25, 2.5)
		if err := b.initJournal(path); err != nil {
			t.Fatal(err)
		}
		if err := b.closeJournal(); err != nil {
			t.Fatal(err)
		}
		other := journalBudgetForTest(t, cfg[0], cfg[1], cfg[2])
		if err := other.enableJournal(path); !errors.Is(err, ErrBudgetUsageUnavailable) {
			t.Fatalf("changed allowance/prices accepted: %v", err)
		}
	}
}

func TestBudgetJournalReservePersistenceFailureNeverNetworks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	defer b.closeJournal()
	// Replacing the target with a directory causes publication to fail even
	// when the tests run as root. No injected production write hooks needed.
	blockJournalPublication(t, path)
	var calls atomic.Int32
	srv := journalServerForTest(t, &calls, nil, true)
	c, err := chat.New(chat.Config{BaseURL: srv.URL, Model: "fixture", Reasoning: "none"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := completeText(context.Background(), c, "private prompt", "hello", 8, b); !errors.Is(err, ErrBudgetUsageUnavailable) {
			t.Fatalf("failed reservation publication did not stop: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("unpersisted reservation reached network %d times", calls.Load())
	}
}

func TestBudgetJournalSettlementPersistenceFailureStopsLaterCalls(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "budget.json")
	b := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	defer b.closeJournal()
	var calls atomic.Int32
	errCh := make(chan error, 1)
	srv := journalServerForTest(t, &calls, func() {
		if err := os.Rename(dir, dir+"-saved"); err != nil {
			errCh <- err
			return
		}
		errCh <- os.WriteFile(dir, []byte("block publication"), 0o600)
	}, true)
	c, err := chat.New(chat.Config{BaseURL: srv.URL, Model: "fixture", Reasoning: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completeText(context.Background(), c, "", "hello", 8, b); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("failed settlement publication did not stop: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if _, err := completeText(context.Background(), c, "", "hello", 8, b); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("failed settlement allowed a later request: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("settlement failure reached network %d times", calls.Load())
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir+"-saved", dir); err != nil {
		t.Fatal(err)
	}
	if err := b.closeJournal(); err != nil {
		t.Fatal(err)
	}
	restarted := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := restarted.enableJournal(path); !errors.Is(err, ErrBudgetUsageUnavailable) || restarted.reserved < 0.05 {
		t.Fatalf("failed settlement did not leave a durable pending reservation: reserved=%g err=%v", restarted.reserved, err)
	}
}

func TestBudgetJournalUnknownUsageDurablyStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv := journalServerForTest(t, &calls, nil, false)
	c, err := chat.New(chat.Config{BaseURL: srv.URL, Model: "fixture", Reasoning: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completeText(context.Background(), c, "private prompt", "secret fixture text", 8, b); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("unknown usage accepted: %v", err)
	}
	if err := b.closeJournal(); err != nil {
		t.Fatal(err)
	}
	restarted := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := restarted.enableJournal(path); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("unknown usage was forgotten on restart: %v", err)
	}
	if _, err := completeText(context.Background(), c, "", "hello", 8, restarted); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("unknown usage allowed another request: %v", err)
	}
	if calls.Load() != 1 || restarted.SpentUSD() < 0.05 {
		t.Fatalf("restart lost uncertain spend: calls=%d spent=%g", calls.Load(), restarted.SpentUSD())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private prompt") || strings.Contains(string(data), "secret fixture text") {
		t.Fatal("journal contains prompt data")
	}
}

func TestBudgetJournalReservationIsDurableBeforeNetworking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	defer b.closeJournal()
	observed := make(chan budgetRecord, 1)
	errCh := make(chan error, 1)
	var calls atomic.Int32
	srv := journalServerForTest(t, &calls, func() {
		var rec budgetRecord
		errCh <- readPrivateJSON(path, &rec)
		observed <- rec
	}, true)
	c, err := chat.New(chat.Config{BaseURL: srv.URL, Model: "fixture", Reasoning: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completeText(context.Background(), c, "", "hello", 8, b); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if rec := <-observed; rec.State.PendingCalls != 1 || rec.State.Reserved < 0.05 || rec.State.Calls != 0 {
		t.Fatalf("network preceded the reservation checkpoint: %+v", rec.State)
	}
	var settled budgetRecord
	if err := readPrivateJSON(path, &settled); err != nil {
		t.Fatal(err)
	}
	if settled.State.PendingCalls != 0 || settled.State.Reserved != 0 || settled.State.Calls != 1 || math.Abs(settled.State.Spent-0.0015) > 1e-12 {
		t.Fatalf("settlement did not publish before returning: %+v", settled.State)
	}
	for _, name := range []string{path, path + ".lock"} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("journal and lock must be private: %v %v", info, err)
		}
	}
}

func TestBudgetJournalInvalidStateRejected(t *testing.T) {
	for _, corruption := range []string{"negative spend", "negative reserved", "negative calls", "orphan reserved", "lost spend", "invalid stop", "unsupported version", "nonfinite", "checksum", "public", "symlink"} {
		t.Run(corruption, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "budget.json")
			b := journalBudgetForTest(t, 1, 1.25, 2.5)
			if err := b.initJournal(path); err != nil {
				t.Fatal(err)
			}
			reserved, err := b.reserve("", "hello", 8)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.settle(reserved, chat.Usage{PromptTokens: 1000, CompletionTokens: 100}); err != nil {
				t.Fatal(err)
			}
			if err := b.closeJournal(); err != nil {
				t.Fatal(err)
			}
			var rec budgetRecord
			if err := readPrivateJSON(path, &rec); err != nil {
				t.Fatal(err)
			}
			switch corruption {
			case "negative spend":
				rec.State.Spent = -1
			case "negative reserved":
				rec.State.Reserved = -1
			case "negative calls":
				rec.State.Calls = -1
			case "orphan reserved":
				rec.State.Reserved = 0.05
			case "lost spend":
				rec.State.Spent = 0
			case "invalid stop":
				rec.State.Stop = "ignore"
			case "unsupported version":
				rec.State.Version = 2
			}
			encoded, err := json.Marshal(rec.State)
			if err != nil {
				t.Fatal(err)
			}
			rec.StateSHA256 = sha256Hex(encoded)
			if corruption == "checksum" {
				rec.StateSHA256 = strings.Repeat("0", 64)
			}
			data, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			if corruption == "nonfinite" {
				data = []byte(strings.Replace(string(data), `"spent_usd":0.0015`, `"spent_usd":1e999`, 1))
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if corruption == "public" {
				err = os.Chmod(path, 0o644)
			}
			if corruption == "symlink" {
				if err = os.Rename(path, path+".backup"); err == nil {
					err = os.Symlink(path+".backup", path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			restarted := journalBudgetForTest(t, 1, 1.25, 2.5)
			if err := restarted.enableJournal(path); !errors.Is(err, ErrBudgetUsageUnavailable) {
				t.Fatalf("invalid state accepted: %v", err)
			}
			if _, err := restarted.reserve("", "hello", 8); !errors.Is(err, ErrBudgetUsageUnavailable) {
				t.Fatalf("invalid state allowed paid request: %v", err)
			}
		})
	}
}

func TestBudgetJournalInitializationNeverResetsAllowance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	if err := b.closeJournal(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.reserve("", "hello", 8); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("closed journal allowed request: %v", err)
	}
	for range 2 {
		other := journalBudgetForTest(t, 1, 1.25, 2.5)
		if err := other.initJournal(path); !errors.Is(err, ErrBudgetUsageUnavailable) {
			t.Fatalf("existing allowance reset: %v", err)
		}
		// A lost journal with its durable lock marker must not be initialized.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func TestBudgetJournalNewPublicationNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	const original = "existing allowance must remain intact\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeNewDurableJSON(path, []byte(`{"new":"allowance"}`)); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive initialization publisher accepted an existing path: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != original {
		t.Fatalf("initializer publication replaced an existing allowance: %q %v", data, err)
	}
}

func TestBudgetJournalConcurrentSettlementsSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := journalBudgetForTest(t, 2, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reserved, err := b.reserve("", "hello", 8)
			if err != nil {
				t.Error(err)
				return
			}
			if err := b.settle(reserved, chat.Usage{PromptTokens: 1000, CompletionTokens: 100}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := b.closeJournal(); err != nil {
		t.Fatal(err)
	}
	b = journalBudgetForTest(t, 2, 1.25, 2.5)
	if err := b.enableJournal(path); err != nil {
		t.Fatal(err)
	}
	defer b.closeJournal()
	if b.calls != 20 || b.prompt != 20000 || b.completion != 2000 || b.reserved != 0 || math.Abs(b.SpentUSD()-0.03) > 1e-12 {
		t.Fatalf("concurrent settled usage lost on restart: %s reserved=%g", b.Note(), b.reserved)
	}
}

// A second already-reserved request must still settle durably after the first
// stops the budget. Its unknown usage can conservatively exceed the cap; that
// is stopped accounting, not permission to make another request.
func TestBudgetJournalConcurrentStopSettlementsRemainDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	defer b.closeJournal()
	first, err := b.reserve("", "first", 8)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.reserve("", "second", 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.settle(first, chat.Usage{PromptTokens: 1000000, CompletionTokens: 1}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("underestimated reservation did not stop: %v", err)
	}
	if err := b.settle(second, chat.Usage{}); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("unknown usage did not stop: %v", err)
	}
	var rec budgetRecord
	if err := readPrivateJSON(path, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.State.Calls != 2 || rec.State.PendingCalls != 0 || rec.State.Reserved != 0 || rec.State.Stop != "usage_unavailable" || math.Abs(rec.State.Spent-1.3000025) > 1e-12 {
		t.Fatalf("valid stopped settlement was not journaled: %+v", rec.State)
	}
}

func TestBudgetJournalUnknownThenUnderestimatedSettlementRemainsDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.closeJournal() })
	first, err := b.reserve("", "first", 8)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.reserve("", "second", 8)
	if err != nil {
		t.Fatal(err)
	}
	// Both requests are already reserved. Force the missing settlement order
	// across goroutines rather than relying on scheduler timing under -race.
	unknownSettled := make(chan struct{})
	settlements := make(chan error, 2)
	go func() {
		settlements <- b.settle(first, chat.Usage{})
		close(unknownSettled)
	}()
	go func() {
		<-unknownSettled
		settlements <- b.settle(second, chat.Usage{PromptTokens: 1000000, CompletionTokens: 1})
	}()
	for range 2 {
		if err := <-settlements; !errors.Is(err, ErrBudgetUsageUnavailable) {
			t.Errorf("unknown-usage stop lost precedence: %v", err)
		}
	}
	var rec budgetRecord
	if err := readPrivateJSON(path, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.State.Calls != 2 || rec.State.PendingCalls != 0 || rec.State.Reserved != 0 ||
		rec.State.PromptTokens != 1000000 || rec.State.CompletionTokens != 1 ||
		rec.State.Stop != "usage_unavailable" || math.Abs(rec.State.Spent-1.3000025) > 1e-12 {
		t.Errorf("known settlement after unknown usage was not journaled: %+v", rec.State)
	}
	if _, err := b.reserve("", "later", 8); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Errorf("stopped budget allowed another request: %v", err)
	}
	if err := b.closeJournal(); err != nil {
		t.Fatal(err)
	}
	restarted := journalBudgetForTest(t, 1, 1.25, 2.5)
	t.Cleanup(func() { restarted.closeJournal() })
	if err := restarted.enableJournal(path); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("restart forgot unknown-usage stop: %v", err)
	}
	if restarted.calls != 2 || restarted.pending != 0 || restarted.reserved != 0 ||
		restarted.prompt != 1000000 || restarted.completion != 1 ||
		math.Abs(restarted.SpentUSD()-1.3000025) > 1e-12 {
		t.Errorf("restart lost a completed settlement: %s pending=%d reserved=%g", restarted.Note(), restarted.pending, restarted.reserved)
	}
	if _, err := restarted.reserve("", "later", 8); !errors.Is(err, ErrBudgetUsageUnavailable) {
		t.Fatalf("restart allowed another request: %v", err)
	}
}

func TestBudgetJournalUnderestimatedUsageDurablyStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	reserved, err := b.reserve("", "hello", 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.settle(reserved, chat.Usage{PromptTokens: 100000, CompletionTokens: 1}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("underestimated reservation did not stop: %v", err)
	}
	if err := b.closeJournal(); err != nil {
		t.Fatal(err)
	}
	b = journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.enableJournal(path); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("underestimated usage stop was forgotten: %v", err)
	}
	if _, err := b.reserve("", "hello", 8); !errors.Is(err, ErrBudgetExceeded) || math.Abs(b.SpentUSD()-0.1250025) > 1e-12 {
		t.Fatalf("prior stop allowed another request or reset spend: spent=%g err=%v", b.SpentUSD(), err)
	}
}

func TestBudgetJournalExclusiveAcrossProcesses(t *testing.T) {
	if path := os.Getenv("KENFOLD_BUDGET_LOCK_FIXTURE_PATH"); path != "" {
		b := journalBudgetForTest(t, 1, 1.25, 2.5)
		if err := b.enableJournal(path); !errors.Is(err, ErrBudgetUsageUnavailable) || !strings.Contains(err.Error(), "locked") {
			t.Fatalf("another process acquired the allowance: %v", err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "budget.json")
	b := journalBudgetForTest(t, 1, 1.25, 2.5)
	if err := b.initJournal(path); err != nil {
		t.Fatal(err)
	}
	defer b.closeJournal()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBudgetJournalExclusiveAcrossProcesses$")
	cmd.Env = append(os.Environ(), "KENFOLD_BUDGET_LOCK_FIXTURE_PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("process lock fixture failed: %v\n%s", err, out)
	}
}

func blockJournalPublication(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func journalServerForTest(t *testing.T, calls *atomic.Int32, beforeReply func(), usage bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if beforeReply != nil {
			beforeReply()
		}
		if usage {
			fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`)
		} else {
			fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func journalBudgetForTest(t *testing.T, limit, input, output float64) *Budget {
	t.Helper()
	b, err := NewBudget(limit, input, output)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
