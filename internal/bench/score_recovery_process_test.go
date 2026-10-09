package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/embed"
	"github.com/kenfold/kenfold/internal/store"
)

type scoreChildResult struct {
	Rows       []Row   `json:"rows"`
	Error      string  `json:"error"`
	JournalErr string  `json:"journal_error"`
	Calls      int     `json:"calls"`
	Spent      float64 `json:"spent"`
}

// The helper is the real scoreQuestions loop in a fresh test process. All event
// blocking and fixture plumbing is test-only; no runtime callback is added.
func TestScoreRecoveryProcessChild(t *testing.T) {
	dir := os.Getenv("KENFOLD_SCORE_CHILD_DIR")
	if dir == "" {
		t.Skip("subprocess fixture only")
	}
	endpoint := os.Getenv("KENFOLD_SCORE_CHILD_HTTP")
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil {
		t.Fatal("child requires a credential-free loopback fixture")
	}
	pool := scoreFixturePool(t)
	c, err := NewScoreCache(filepath.Join(dir, "scores"), "subprocess-fixture-dataset-memory-code-inference-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	mode := os.Getenv("KENFOLD_SCORE_CHILD_MODE")
	limit := 1.0
	if mode == "reader-done" {
		limit = 0.05
	}
	b, err := NewBudget(limit, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	journalErr := b.enableJournal(filepath.Join(dir, "budget.json"))
	defer b.closeJournal()
	reader, err := chat.New(chat.Config{BaseURL: endpoint, Model: "fixture-reader", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	judge, err := chat.New(chat.Config{BaseURL: endpoint, Model: "fixture-judge", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	emb, err := embed.New(embed.Config{BaseURL: endpoint, Model: "fixture-embedding", Dim: store.EmbeddingDim})
	if err != nil {
		t.Fatal(err)
	}
	var qs []scoredQ
	if err := readPrivateJSON(filepath.Join(dir, "questions.json"), &qs); err != nil {
		t.Fatal(err)
	}
	d := Deps{Store: store.New(pool), Pool: pool, Embedder: emb, Reader: reader, Judge: judge, ScoreCache: c, Budget: b}
	block := func(event string) {
		fmt.Println("SCORE_EVENT " + event)
		// The parent owns the open stdin pipe and kills us while this blocks.
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatal("fixture event pipe closed before kill")
		}
	}
	rows, runErr := scoreQuestions(context.Background(), d, qs, true, func(format string, args ...any) {
		if mode == "completed" && os.Getenv("KENFOLD_SCORE_CHILD_PAUSE") == "1" && fmt.Sprintf(format, args...) == "scored 25/26" {
			block("completed")
		}
	})
	if mode == "reader-done" && os.Getenv("KENFOLD_SCORE_CHILD_PAUSE") == "1" {
		if !errors.Is(runErr, ErrBudgetExceeded) {
			t.Fatalf("reader-done boundary was not a pre-network reservation stop: %v", runErr)
		}
		block("reader-done")
	}
	result := scoreChildResult{Rows: rows, Calls: b.calls, Spent: b.SpentUSD()}
	if runErr != nil {
		result.Error = runErr.Error()
	}
	if journalErr != nil {
		result.JournalErr = journalErr.Error()
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDurableJSON(filepath.Join(dir, "child-result.json"), data); err != nil {
		t.Fatal(err)
	}
}

type runningScoreChild struct {
	cmd    *exec.Cmd
	events <-chan string
	done   <-chan error
	output *bytes.Buffer
}

func startScoreChild(t *testing.T, dir, endpoint, mode string, pause bool) *runningScoreChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestScoreRecoveryProcessChild$", "-test.timeout=45s")
	cmd.Env = append(scoreChildEnv(), "KENFOLD_SCORE_CHILD_DIR="+dir, "KENFOLD_SCORE_CHILD_HTTP="+endpoint, "KENFOLD_SCORE_CHILD_MODE="+mode)
	if pause {
		cmd.Env = append(cmd.Env, "KENFOLD_SCORE_CHILD_PAUSE=1")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	events := make(chan string, 8)
	done := make(chan error, 1)
	go func() {
		scan := bufio.NewScanner(stdout)
		for scan.Scan() {
			line := scan.Text()
			if strings.HasPrefix(line, "SCORE_EVENT ") {
				events <- strings.TrimPrefix(line, "SCORE_EVENT ")
			}
		}
		// Drain stdout before Wait closes the descriptor.
		done <- cmd.Wait()
		close(events)
	}()
	t.Cleanup(func() { cmd.Process.Kill(); stdin.Close() })
	return &runningScoreChild{cmd: cmd, events: events, done: done, output: output}
}

func (c *runningScoreChild) event(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-c.events:
		if got != want {
			t.Fatalf("child event = %q, want %q", got, want)
		}
	case err := <-c.done:
		t.Fatalf("child stopped before event: %v\n%s", err, c.output)
	case <-time.After(20 * time.Second):
		t.Fatal("child did not reach controlled checkpoint boundary")
	}
}

func (c *runningScoreChild) kill(t *testing.T) {
	t.Helper()
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-c.done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != -1 {
			t.Fatalf("child was not killed by signal: %v\n%s", err, c.output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("killed child failed to exit")
	}
}

func (c *runningScoreChild) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatalf("restart child failed: %v\n%s", err, c.output)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("restart child failed to finish")
	}
}

func prepareScoreChild(t *testing.T, qs []scoredQ, limit float64) string {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(qs)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDurableJSON(filepath.Join(dir, "questions.json"), data); err != nil {
		t.Fatal(err)
	}
	b, err := NewBudget(limit, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.initJournal(filepath.Join(dir, "budget.json")); err != nil {
		t.Fatal(err)
	}
	if err := b.closeJournal(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readScoreChildResult(t *testing.T, dir string) scoreChildResult {
	t.Helper()
	var result scoreChildResult
	if err := readPrivateJSON(filepath.Join(dir, "child-result.json"), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestScoreRecoveryProcessKillCompletedAndResume(t *testing.T) {
	pool := scoreFixturePool(t)
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, pool, f)
	seed := scoreFixtureQuestions(t, d)[0]
	qs := make([]scoredQ, 26)
	for i := range qs {
		qs[i] = seed
		qs[i].ID = fmt.Sprintf("q-%02d", i)
	}
	dir := prepareScoreChild(t, qs, 1)
	first := startScoreChild(t, dir, f.server.URL, "completed", true)
	first.event(t, "completed")
	if f.reader.Load() != 25 || f.judge.Load() != 25 {
		t.Fatal("completion event preceded actual answer/judge calls")
	}
	var ledger budgetRecord
	if err := readPrivateJSON(filepath.Join(dir, "budget.json"), &ledger); err != nil {
		t.Fatal(err)
	}
	if ledger.State.Calls != 50 || ledger.State.PendingCalls != 0 {
		t.Fatal("progress preceded durable budget settlement")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "scores", "question-*.json"))
	if err != nil || len(paths) != 25 {
		t.Fatal("progress preceded durable completed checkpoints")
	}
	for _, path := range paths {
		var rec scoreRecord
		if err := readPrivateJSON(path, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.State.Phase != scoreDone || rec.State.Judge.Text != "yes" || !rec.State.Judge.Yes || rec.State.Reader.Answer != "blue" || rec.State.Reader.Metrics != (scoreMetrics{F1: 1, Recall5: 1, Recall10: 1}) {
			t.Fatal("fixture verdict/F1/recall not durably scored")
		}
	}
	first.kill(t)
	// Only the unfinished question may see the changed fixture responses.
	f.mu.Lock()
	f.answer, f.verdict = "red", "no"
	f.mu.Unlock()
	reader, judge, embedding := f.reader.Load(), f.judge.Load(), f.embedding.Load()
	second := startScoreChild(t, dir, f.server.URL, "completed", false)
	second.wait(t)
	got := readScoreChildResult(t, dir)
	want := []Row{
		{Label: "all", N: 26, Judge: 25, F1: 25, HasF1: true, Recall5: 26, Recall10: 26, RecallN: 26},
		{Label: LoCoMoCategory("2"), N: 26, Judge: 25, F1: 25, HasF1: true, Recall5: 26, Recall10: 26, RecallN: 26},
	}
	if got.Error != "" || got.JournalErr != "" || !reflect.DeepEqual(got.Rows, want) || got.Calls != 52 || math.Abs(got.Spent-0.001092) > 1e-12 {
		t.Fatalf("restart lost verdict/metrics/accounting: %+v", got)
	}
	if f.reader.Load()-reader != 1 || f.judge.Load()-judge != 1 || f.embedding.Load()-embedding != 1 {
		t.Fatal("restart duplicated completed question HTTP/retrieval")
	}
	reader, judge, embedding = f.reader.Load(), f.judge.Load(), f.embedding.Load()
	third := startScoreChild(t, dir, f.server.URL, "completed", false)
	third.wait(t)
	replay := readScoreChildResult(t, dir)
	if !reflect.DeepEqual(replay, got) || f.reader.Load() != reader || f.judge.Load() != judge || f.embedding.Load() != embedding {
		t.Fatal("fully completed subprocess replay duplicated calls/accumulators")
	}
	t.Log("SIGKILL after 25 durable results; same-cache/journal restart made only 1 new reader + judge + search; full replay made 0 calls")
}

func TestScoreRecoveryProcessKillReaderDoneAndResume(t *testing.T) {
	pool := scoreFixturePool(t)
	f := newScoreHTTPFixture(t)
	d := scoreFixtureDeps(t, pool, f)
	qs := scoreFixtureQuestions(t, d)[:1]
	dir := prepareScoreChild(t, qs, 0.05)
	first := startScoreChild(t, dir, f.server.URL, "reader-done", true)
	first.event(t, "reader-done")
	path := scoreQuestionRecordPath(t, filepath.Join(dir, "scores"))
	var rec scoreRecord
	if err := readPrivateJSON(path, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.State.Phase != scoreReaderDone || rec.State.Reader.Answer != "blue" || rec.State.Judge != nil {
		t.Fatal("blocked reservation was incorrectly marked judge-in-flight")
	}
	first.kill(t)
	reader, judge, embedding := f.reader.Load(), f.judge.Load(), f.embedding.Load()
	second := startScoreChild(t, dir, f.server.URL, "reader-done", false)
	second.wait(t)
	got := readScoreChildResult(t, dir)
	if !strings.Contains(got.Error, ErrBudgetExceeded.Error()) || strings.Contains(got.Error, "in-flight") || got.JournalErr != "" || len(got.Rows) != 0 || got.Calls != 1 {
		t.Fatalf("reader-done reservation cutoff became an uncertain call: %+v", got)
	}
	if f.reader.Load() != reader || f.judge.Load() != judge || f.embedding.Load() != embedding {
		t.Fatal("reader-done restart retried reader/search or bypassed budget")
	}
	t.Log("SIGKILL at reader-done; same exhausted journal reopened cleanly; judge stayed blocked before network; reader/search duplicated 0 times")
}

func TestScoreRecoveryProcessKillUnknownReaderAndJudge(t *testing.T) {
	pool := scoreFixturePool(t)
	for _, model := range []string{"fixture-reader", "fixture-judge"} {
		t.Run(model, func(t *testing.T) {
			f := newScoreHTTPFixture(t)
			d := scoreFixtureDeps(t, pool, f)
			qs := scoreFixtureQuestions(t, d)[:1]
			dir := prepareScoreChild(t, qs, 1)
			entered, release := make(chan struct{}, 1), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			f.mu.Lock()
			f.beforeChat = func(called string) {
				if called == model {
					select {
					case entered <- struct{}{}:
					default:
					}
					<-release
				}
			}
			f.mu.Unlock()
			first := startScoreChild(t, dir, f.server.URL, "unknown", false)
			select {
			case <-entered:
			case <-time.After(20 * time.Second):
				t.Fatal("child did not enter controlled in-flight HTTP call")
			}
			var rec scoreRecord
			if err := readPrivateJSON(scoreQuestionRecordPath(t, filepath.Join(dir, "scores")), &rec); err != nil {
				t.Fatal(err)
			}
			phase := scoreReaderInFlight
			if model == "fixture-judge" {
				phase = scoreJudgeInFlight
			}
			if rec.State.Phase != phase || (model == "fixture-reader" && rec.State.Reader != nil) || (model == "fixture-judge" && rec.State.Reader == nil) {
				t.Fatal("in-flight reader/judge phases were conflated")
			}
			first.kill(t)
			unblock()
			reader, judge, embedding := f.reader.Load(), f.judge.Load(), f.embedding.Load()
			second := startScoreChild(t, dir, f.server.URL, "unknown", false)
			second.wait(t)
			got := readScoreChildResult(t, dir)
			if !strings.Contains(got.Error, "in-flight") || !strings.Contains(got.Error, phase) || !strings.Contains(got.JournalErr, "unsettled reservation") || len(got.Rows) != 0 {
				t.Fatalf("unknown call did not fail closed with same journal: %+v", got)
			}
			if f.reader.Load() != reader || f.judge.Load() != judge || f.embedding.Load() != embedding {
				t.Fatal("subprocess restart blindly retried an unknown call")
			}
			t.Logf("SIGKILL during %s; same cache/journal reopen made 0 retrieval/reader/judge calls", phase)
		})
	}
}
