package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
)

type budgetJournal struct {
	path string
	lock *os.File
}

// A journal is a durably replaced snapshot, not a transcript or a billing log.
// PendingCalls distinguishes an in-flight request even if floating point
// arithmetic would leave a negligible residual reservation.
type budgetState struct {
	Version          int     `json:"version"`
	Limit            float64 `json:"limit_usd"`
	Input            float64 `json:"input_usd_per_million"`
	Output           float64 `json:"output_usd_per_million"`
	Spent            float64 `json:"spent_usd"`
	Reserved         float64 `json:"reserved_usd"`
	PendingCalls     int     `json:"pending_calls"`
	Calls            int     `json:"calls"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	Stop             string  `json:"stop"`
}

type budgetRecord struct {
	State       budgetState `json:"state"`
	StateSHA256 string      `json:"state_sha256"`
}

// initJournal explicitly creates a NEW allowance and enables it. It must be
// called before any budgeted work and refuses existing journal or lock paths.
// Normal restarts must use enableJournal; they must never auto-initialize.
func (b *Budget) initJournal(path string) error { return b.openJournal(path, true) }

// enableJournal resumes an existing, settled allowance. Any failure latches
// fail-closed even if the caller accidentally ignores the returned error.
func (b *Budget) enableJournal(path string) error { return b.openJournal(path, false) }

func (b *Budget) openJournal(path string, initialize bool) error {
	if b == nil {
		return fmt.Errorf("%w: journaling requires a budget", ErrBudgetUsageUnavailable)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	fail := func(err error) error { return b.journalFailure(err) }
	if b.journal != nil || b.spent != 0 || b.reserved != 0 || b.calls != 0 || b.stopped != nil {
		return fail(errors.New("journal must be enabled on a fresh budget before requests"))
	}
	if path == "" {
		return fail(errors.New("journal path is required"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fail(err)
	}
	// Canonicalize directory aliases so they contend for the same lock. The
	// journal and lock themselves must not be symlinks.
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return fail(err)
	}
	path = filepath.Join(dir, filepath.Base(abs))
	lockPath := path + ".lock"
	if initialize {
		for _, p := range []string{path, lockPath} {
			if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
				return fail(fmt.Errorf("new journal requires unused journal and lock paths: %s", p))
			}
		}
	}
	flags := os.O_CREATE | os.O_RDWR | syscall.O_NOFOLLOW
	if initialize {
		flags |= os.O_EXCL
	}
	lock, err := os.OpenFile(lockPath, flags, 0o600)
	if err != nil {
		return fail(err)
	}
	attached := false
	defer func() {
		if !attached {
			lock.Close()
		}
	}()
	info, err := lock.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fail(errors.New("budget journal lock must be an owner-only regular file"))
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("budget journal is already locked or cannot be locked: %w", err))
	}
	journal := &budgetJournal{path: path, lock: lock}
	if initialize {
		// Recheck under the lock; never replace an existing allowance.
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fail(errors.New("budget journal already exists"))
		}
		b.journal = journal
		if err := b.publishJournal(true); err != nil {
			b.journal = nil
			return fail(err)
		}
	} else {
		var rec budgetRecord
		if err := readPrivateJSON(path, &rec); err != nil {
			return fail(fmt.Errorf("read budget journal: %w", err))
		}
		encoded, err := json.Marshal(rec.State)
		if err != nil || sha256Hex(encoded) != rec.StateSHA256 {
			return fail(errors.New("budget journal checksum mismatch"))
		}
		if err := validateBudgetState(rec.State); err != nil {
			return fail(err)
		}
		s := rec.State
		if s.Limit != b.limit || s.Input != b.input || s.Output != b.output {
			return fail(errors.New("budget journal limit or prices differ from configuration"))
		}
		b.spent, b.reserved, b.pending = s.Spent, s.Reserved, s.PendingCalls
		b.calls, b.prompt, b.completion = s.Calls, s.PromptTokens, s.CompletionTokens
		if s.PendingCalls != 0 {
			return fail(errors.New("budget journal has an unsettled reservation; request charge is unknown"))
		}
		if s.Stop == "usage_unavailable" {
			return fail(errors.New("budget journal previously stopped with unknown usage"))
		}
		if s.Stop == "budget_exceeded" {
			b.stopped = fmt.Errorf("%w: journal previously stopped after an underestimated reservation", ErrBudgetExceeded)
			return b.stopped
		}
		b.journal = journal
	}
	attached = true
	return nil
}

// closeJournal releases the process-lifetime lock. This budget must not be
// used for networking again; reopening requires a fresh Budget.
func (b *Budget) closeJournal() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.journal == nil || b.journal.lock == nil {
		return nil
	}
	err := b.journal.lock.Close()
	b.journal.lock = nil
	if err != nil {
		return b.journalFailure(err)
	}
	return nil
}

// These helpers are called only while b.mu is held, across reservation /
// settlement and their publication. No subsequent reserve can race fsync.
func (b *Budget) journalFailure(err error) error {
	b.stopped = fmt.Errorf("%w: budget journal: %v", ErrBudgetUsageUnavailable, err)
	return b.stopped
}

func (b *Budget) persistJournal() error {
	return b.publishJournal(false)
}

func (b *Budget) publishJournal(exclusive bool) error {
	if b.journal == nil {
		return nil
	}
	if b.journal.lock == nil {
		return errors.New("budget journal is closed")
	}
	s := budgetState{
		Version: 1, Limit: b.limit, Input: b.input, Output: b.output,
		Spent: b.spent, Reserved: b.reserved, PendingCalls: b.pending,
		Calls: b.calls, PromptTokens: b.prompt, CompletionTokens: b.completion,
	}
	if b.stopped != nil {
		if errors.Is(b.stopped, ErrBudgetExceeded) {
			s.Stop = "budget_exceeded"
		} else {
			s.Stop = "usage_unavailable"
		}
	}
	if err := validateBudgetState(s); err != nil {
		return err
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return err
	}
	data, err := json.Marshal(budgetRecord{State: s, StateSHA256: sha256Hex(encoded)})
	if err != nil {
		return err
	}
	if exclusive {
		return writeNewDurableJSON(b.journal.path, data)
	}
	return writeDurableJSON(b.journal.path, data)
}

func validateBudgetState(s budgetState) error {
	if s.Version != 1 {
		return errors.New("unsupported budget journal version")
	}
	for _, v := range []float64{s.Limit, s.Input, s.Output} {
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("invalid budget journal configuration")
		}
	}
	for _, v := range []float64{s.Spent, s.Reserved} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("invalid budget journal charge")
		}
	}
	if s.PendingCalls < 0 || s.Calls < 0 || s.PromptTokens < 0 || s.CompletionTokens < 0 || (s.PendingCalls == 0) != (s.Reserved == 0) {
		return errors.New("invalid budget journal counters or pending reservation")
	}
	if s.Stop != "" && s.Stop != "usage_unavailable" && s.Stop != "budget_exceeded" {
		return errors.New("invalid budget journal stop state")
	}
	known := float64(s.PromptTokens)*s.Input/1e6 + float64(s.CompletionTokens)*s.Output/1e6
	tolerance := 1e-12 * math.Max(1, math.Max(known, s.Spent))
	if math.IsNaN(known) || math.IsInf(known, 0) || s.Spent < known-tolerance || (s.Stop != "usage_unavailable" && math.Abs(s.Spent-known) > tolerance) {
		return errors.New("budget journal spend disagrees with token counters")
	}
	if s.Stop != "usage_unavailable" && (s.PromptTokens < s.Calls || s.CompletionTokens < s.Calls) {
		return errors.New("budget journal settled calls lack token usage")
	}
	if s.Calls == 0 && (s.Spent != 0 || s.PromptTokens != 0 || s.CompletionTokens != 0) {
		return errors.New("budget journal charges without settled calls")
	}
	if s.Stop == "" && s.Spent+s.Reserved > s.Limit+tolerance {
		return errors.New("budget journal exceeds its allowance without a stop")
	}
	return nil
}
