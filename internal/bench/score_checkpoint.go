package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/store"
)

var (
	ErrScoreCheckpoint  = errors.New("benchmark score checkpoint failed; stopping")
	ErrScoreCallUnknown = errors.New("benchmark score checkpoint has an in-flight call with unknown outcome; automatic retry is disabled")
)

const (
	scoreReaderInFlight = "reader-in-flight"
	scoreReaderDone     = "reader-done"
	scoreJudgeInFlight  = "judge-in-flight"
	scoreDone           = "scored"
)

// ScoreCache is an opt-in, exclusively locked scoring directory. Identity must
// externally pin dataset bytes, input/extracted memory state, retrieval,
// embedding/reranking, scoring code/options, weights and inference profiles.
// Model names alone are insufficient. Only its SHA-256 is persisted.
type ScoreCache struct {
	mu       sync.Mutex
	dir      string
	identity string
	dirInfo  os.FileInfo
	lockInfo os.FileInfo
	lock     *os.File
	stopped  error
}

type scoreNamespace struct {
	Version        int    `json:"version"`
	IdentitySHA256 string `json:"identity_sha256"`
}

// A slot is scope + ID, so changed question/model/options cannot silently miss
// an existing result. Raw, extracted and LME scopes remain separate slots.
type scoreKey struct {
	Version            int      `json:"version"`
	IdentitySHA256     string   `json:"identity_sha256"`
	Scope              string   `json:"scope"`
	ID                 string   `json:"id"`
	Type               string   `json:"type"`
	Text               string   `json:"text"`
	Gold               string   `json:"gold"`
	Evidence           []string `json:"evidence"`
	Abstain            bool     `json:"abstain"`
	When               string   `json:"when"`
	WithF1             bool     `json:"with_f1"`
	ReaderModel        string   `json:"reader_model"`
	JudgeModel         string   `json:"judge_model"`
	ReaderEndpointHash string   `json:"reader_endpoint_sha256"`
	JudgeEndpointHash  string   `json:"judge_endpoint_sha256"`
	ReaderPromptHash   string   `json:"reader_prompt_sha256"`
	JudgePromptHash    string   `json:"judge_prompt_sha256"`
	ReaderTokens       int      `json:"reader_tokens"`
	JudgeTokens        int      `json:"judge_tokens"`
	TopK               int      `json:"top_k"`
	ReaderPolicy       string   `json:"reader_policy,omitempty"`
	ContextPolicy      string   `json:"context_policy,omitempty"`
	CaptureInputs      bool     `json:"capture_inputs,omitempty"`
}

type scoreMetrics struct {
	F1            float64  `json:"f1"`
	Recall5       float64  `json:"recall5"`
	Recall10      float64  `json:"recall10"`
	ContextRecall *float64 `json:"reader_context_recall,omitempty"`
}

type scoreReader struct {
	Answer  string       `json:"answer"`
	Metrics scoreMetrics `json:"metrics"`
}

type scoreJudge struct {
	Text string `json:"text"`
	Yes  bool   `json:"yes"`
}

type scoreState struct {
	Key    scoreKey            `json:"key"`
	Phase  string              `json:"phase"`
	Reader *scoreReader        `json:"reader"`
	Judge  *scoreJudge         `json:"judge"`
	Inputs *scoreInputSnapshot `json:"inputs,omitempty"`
}

type scoreInputHit struct {
	ID      string    `json:"id"`
	Scope   string    `json:"scope"`
	DiaID   string    `json:"dia"`
	Session string    `json:"session"`
	Content string    `json:"content"`
	Score   float64   `json:"score"`
	Date    time.Time `json:"date"`
}

type scoreInputSnapshot struct {
	Version       int             `json:"version"`
	Hits          []scoreInputHit `json:"hits"`
	ContextIDs    []string        `json:"context_ids"`
	ReaderSystem  string          `json:"reader_system"`
	ReaderUser    string          `json:"reader_user"`
	ReaderPolicy  string          `json:"reader_policy"`
	ContextPolicy string          `json:"context_policy"`
}

func newScoreInputSnapshot(hits []store.Scored, ids []string, system, user string, opts AnswerOptions) *scoreInputSnapshot {
	s := &scoreInputSnapshot{Version: 1, ContextIDs: append([]string(nil), ids...), ReaderSystem: system, ReaderUser: user, ReaderPolicy: opts.ReaderPolicy, ContextPolicy: opts.ContextPolicy}
	for _, h := range hits {
		s.Hits = append(s.Hits, scoreInputHit{ID: h.ID, Scope: h.Scope, DiaID: attrString(h.Memory, "bench_dia"), Session: attrString(h.Memory, "bench_session"), Content: h.Content, Score: h.Score, Date: h.CreatedAt})
	}
	return s
}

type scoreRecord struct {
	State       scoreState `json:"state"`
	StateSHA256 string     `json:"state_sha256"`
}

func NewScoreCache(dir, identity string) (*ScoreCache, error) {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(identity) == "" {
		return nil, fmt.Errorf("%w: a private directory and nonempty pinned identity are required", ErrScoreCheckpoint)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrScoreCheckpoint, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrScoreCheckpoint, err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrScoreCheckpoint, err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: directory must be private and not a symlink", ErrScoreCheckpoint)
	}
	// Canonicalize parent aliases, but never accept a symlink as the directory.
	dir, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrScoreCheckpoint, err)
	}
	c := &ScoreCache{dir: dir, identity: sha256Hex([]byte(identity)), dirInfo: info}
	lock, err := os.OpenFile(filepath.Join(dir, ".score.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, c.fail(err)
	}
	attached := false
	defer func() {
		if !attached {
			lock.Close()
		}
	}()
	c.lockInfo, err = lock.Stat()
	if err != nil {
		return nil, c.fail(err)
	}
	if !c.lockInfo.Mode().IsRegular() || c.lockInfo.Mode().Perm()&0o077 != 0 {
		return nil, c.fail(errors.New("lock must be an owner-only regular file"))
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, c.fail(fmt.Errorf("directory is already locked or cannot be locked: %w", err))
	}
	c.lock = lock
	path := filepath.Join(dir, "score-cache.json")
	want := scoreNamespace{Version: 1, IdentitySHA256: c.identity}
	var ns scoreNamespace
	if err := readPrivateJSON(path, &ns); errors.Is(err, os.ErrNotExist) {
		data, err := json.Marshal(want)
		if err != nil {
			return nil, c.fail(err)
		}
		if err := writeNewDurableJSON(path, data); err != nil {
			return nil, c.fail(err)
		}
	} else if err != nil {
		return nil, c.fail(fmt.Errorf("read namespace: %w", err))
	} else if ns != want {
		return nil, c.fail(errors.New("incompatible scoring namespace"))
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	attached = true
	return c, nil
}

// Close releases the process-lifetime lock without unlinking it. A closed
// cache cannot be used for scoring again; reopening requires NewScoreCache.
func (c *ScoreCache) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lock == nil {
		return nil
	}
	err := c.lock.Close()
	c.lock = nil
	if err != nil {
		return c.fail(err)
	}
	return nil
}

func (c *ScoreCache) fail(err error) error {
	c.stopped = fmt.Errorf("%w: %w", ErrScoreCheckpoint, err)
	return c.stopped
}

// Called under mu, except during construction before publication to callers.
func (c *ScoreCache) check() error {
	if c.stopped != nil {
		return c.stopped
	}
	if c.lock == nil {
		return c.fail(errors.New("cache is closed"))
	}
	info, err := os.Lstat(c.dir)
	if err != nil {
		return c.fail(err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !os.SameFile(c.dirInfo, info) {
		return c.fail(errors.New("private cache directory was replaced or permissions changed"))
	}
	info, err = os.Lstat(filepath.Join(c.dir, ".score.lock"))
	if err != nil {
		return c.fail(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !os.SameFile(c.lockInfo, info) {
		return c.fail(errors.New("private cache lock was replaced or permissions changed"))
	}
	var ns scoreNamespace
	if err := readPrivateJSON(filepath.Join(c.dir, "score-cache.json"), &ns); err != nil {
		return c.fail(err)
	}
	if ns != (scoreNamespace{Version: 1, IdentitySHA256: c.identity}) {
		return c.fail(errors.New("incompatible scoring namespace"))
	}
	return nil
}

func (c *ScoreCache) key(q scoredQ, withF1 bool, reader, judge *chat.Client) scoreKey {
	return c.keyForOptions(q, withF1, reader, judge, AnswerOptions{})
}

func (c *ScoreCache) keyForOptions(q scoredQ, withF1 bool, reader, judge *chat.Client, opts AnswerOptions) scoreKey {
	opts = effectiveAnswerOptions(q, withF1, opts)
	system, user, _ := readerMessages(q.Question, nil, withF1, opts.ReaderPolicy)
	return scoreKey{
		Version: 1, IdentitySHA256: c.identity, Scope: q.Scope, ID: q.ID,
		Type: q.Type, Text: q.Text, Gold: q.Answer, Evidence: q.Evidence, Abstain: q.Abstain,
		When: q.When.UTC().Format(time.RFC3339Nano), WithF1: withF1,
		ReaderModel: reader.Model(), JudgeModel: judge.Model(),
		ReaderEndpointHash: sha256Hex([]byte(reader.Endpoint())), JudgeEndpointHash: sha256Hex([]byte(judge.Endpoint())),
		ReaderPromptHash: sha256Hex([]byte(system + "\x00" + user)),
		JudgePromptHash:  sha256Hex([]byte(JudgePrompt(q.Question, ""))),
		ReaderTokens:     readerTokens, JudgeTokens: judgeTokens, TopK: topK,
		ReaderPolicy: opts.ReaderPolicy, ContextPolicy: opts.ContextPolicy, CaptureInputs: opts.CaptureInputs,
	}
}

func (c *ScoreCache) path(key scoreKey) (string, error) {
	if key.Version != 1 || key.IdentitySHA256 != c.identity || strings.TrimSpace(key.Scope) == "" || strings.TrimSpace(key.ID) == "" || key.ReaderModel == "" || key.JudgeModel == "" {
		return "", errors.New("invalid or incompatible scoring key")
	}
	data, err := json.Marshal(struct {
		Scope string `json:"scope"`
		ID    string `json:"id"`
	}{key.Scope, key.ID})
	if err != nil {
		return "", err
	}
	return filepath.Join(c.dir, "question-"+sha256Hex(data)+".json"), nil
}

func (c *ScoreCache) load(key scoreKey) (scoreState, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.check(); err != nil {
		return scoreState{}, false, err
	}
	state, hit, err := c.read(key)
	if err != nil {
		return scoreState{}, false, c.fail(err)
	}
	return state, hit, nil
}

func (c *ScoreCache) read(key scoreKey) (scoreState, bool, error) {
	path, err := c.path(key)
	if err != nil {
		return scoreState{}, false, err
	}
	var rec scoreRecord
	if err := readPrivateJSON(path, &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return scoreState{Key: key}, false, nil
		}
		return scoreState{}, false, fmt.Errorf("read question record: %w", err)
	}
	if !reflect.DeepEqual(rec.State.Key, key) {
		return scoreState{}, false, errors.New("incompatible question/model/prompt key")
	}
	data, err := json.Marshal(rec.State)
	if err != nil || sha256Hex(data) != rec.StateSHA256 {
		return scoreState{}, false, errors.New("question record checksum mismatch")
	}
	if err := validateScoreState(rec.State); err != nil {
		return scoreState{}, false, err
	}
	return rec.State, true, nil
}

func (c *ScoreCache) save(state scoreState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.check(); err != nil {
		return err
	}
	if err := validateScoreState(state); err != nil {
		return c.fail(err)
	}
	old, hit, err := c.read(state.Key)
	if err != nil {
		return c.fail(err)
	}
	next := map[string]string{"": scoreReaderInFlight, scoreReaderInFlight: scoreReaderDone, scoreReaderDone: scoreJudgeInFlight, scoreJudgeInFlight: scoreDone}
	if (!hit && state.Phase != scoreReaderInFlight) || (hit && next[old.Phase] != state.Phase) || (old.Reader != nil && !reflect.DeepEqual(old.Reader, state.Reader)) || (hit && !reflect.DeepEqual(old.Inputs, state.Inputs)) {
		return c.fail(errors.New("invalid scoring phase transition or changed saved answer"))
	}
	path, err := c.path(state.Key)
	if err != nil {
		return c.fail(err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return c.fail(err)
	}
	data, err = json.Marshal(scoreRecord{State: state, StateSHA256: sha256Hex(data)})
	if err != nil {
		return c.fail(err)
	}
	if len(data)+1 > 1<<20 {
		return c.fail(errors.New("question record exceeds 1 MiB"))
	}
	if err := writeDurableJSON(path, data); err != nil {
		return c.fail(fmt.Errorf("publish question record: %w", err))
	}
	return nil
}

func validateScoreState(s scoreState) error {
	if s.Key.CaptureInputs {
		if s.Inputs == nil || s.Inputs.Version != 1 || s.Inputs.ReaderSystem == "" || s.Inputs.ReaderUser == "" || s.Inputs.ReaderPolicy != s.Key.ReaderPolicy || s.Inputs.ContextPolicy != s.Key.ContextPolicy {
			return errors.New("missing or incompatible saved reader inputs")
		}
	} else if s.Inputs != nil {
		return errors.New("unexpected saved reader inputs")
	}
	switch s.Phase {
	case scoreReaderInFlight:
		if s.Reader != nil || s.Judge != nil {
			return errors.New("in-flight reader record contains a result")
		}
	case scoreReaderDone, scoreJudgeInFlight:
		if s.Reader == nil || s.Judge != nil {
			return errors.New("reader-done/judge-in-flight record has missing or incompatible results")
		}
	case scoreDone:
		if s.Reader == nil || s.Judge == nil || s.Judge.Yes != JudgeYes(s.Judge.Text) {
			return errors.New("scored record has missing or incompatible results")
		}
	default:
		return errors.New("unsupported scoring phase")
	}
	if s.Reader != nil {
		m := s.Reader.Metrics
		f1 := 0.0
		if s.Key.WithF1 {
			f1 = TokenF1(s.Reader.Answer, s.Key.Gold, s.Key.Type)
		}
		if m.F1 != f1 {
			return errors.New("saved F1 disagrees with the scored answer")
		}
		recalls := []float64{m.Recall5, m.Recall10}
		if (s.Key.ContextPolicy == "neighbors-v1") != (m.ContextRecall != nil) {
			return errors.New("incompatible saved reader context recall")
		}
		if m.ContextRecall != nil {
			if s.Inputs == nil || *m.ContextRecall != evidenceRecall(s.Key.Evidence, s.Inputs.ContextIDs) {
				return errors.New("saved context recall disagrees with reader inputs")
			}
			recalls = append(recalls, *m.ContextRecall)
		}
		for _, r := range recalls {
			if math.IsNaN(r) || math.IsInf(r, 0) || (len(s.Key.Evidence) == 0 && r != -1) || (len(s.Key.Evidence) != 0 && (r < 0 || r > 1)) {
				return errors.New("invalid saved evidence recall")
			}
		}
		if m.Recall10 < m.Recall5 {
			return errors.New("saved recall@10 is less than recall@5")
		}
	}
	return nil
}

// Reserve BEFORE recording in-flight: budget rejection has not made a call.
// Persist in-flight BEFORE Text. Cached calls are single attempts; any failure
// retains that phase and stops. Nil-cache scoring still uses completeText's
// existing retry policy. No budget allowance is initialized/released here.
func (c *ScoreCache) complete(ctx context.Context, state *scoreState, phase string, client *chat.Client, system, user string, maxTokens int, budget *Budget) (string, error) {
	reserved, err := budget.reserve(system, user, maxTokens)
	if err != nil {
		return "", err
	}
	inFlight := *state
	inFlight.Phase = phase
	if err := c.save(inFlight); err != nil {
		return "", err
	}
	*state = inFlight
	text, usage, callErr := client.Text(ctx, system, user, maxTokens)
	if err := budget.settle(reserved, usage); err != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		return "", c.fail(err)
	}
	if callErr != nil {
		return "", c.unknown(phase)
	}
	return strings.TrimSpace(text), nil
}

func (c *ScoreCache) unknown(phase string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fail(fmt.Errorf("%w (%s)", ErrScoreCallUnknown, phase))
}
