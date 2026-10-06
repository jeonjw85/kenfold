// Package hook implements `kenfold hook`, a lifecycle hook for Claude Code and
// Codex. Both clients run it as a command hook and pass the event as JSON on
// stdin; the same binary handles every event:
//
//   - SessionStart: loads shared memory for the current repository (get_context)
//     and returns it as additional context for the model.
//   - UserPromptSubmit, Stop, PostCompact: record the user's requests, the
//     final response, and the compaction summary in a local session log. These
//     events never touch the network, so they add no latency.
//   - SessionEnd: turns the session log into an episodic memory (a short,
//     extractive session summary) and sends it with remember. If the server is
//     unreachable, the summary is spooled and sent at the next SessionStart.
//
// The hook talks to `kenfold serve` over MCP, so it is authenticated by an API
// key like any agent, and every write passes the server's secret filter.
// Captured text is also redacted locally before it is written to disk. Hooks
// fail open: errors are reported on stderr and never block the session.
package hook

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kenfold/kenfold/internal/coderef"
	"github.com/kenfold/kenfold/internal/mcpserver"
	"github.com/kenfold/kenfold/internal/secrets"
)

// DefaultURL is the MCP endpoint of a local `kenfold serve`.
const DefaultURL = "http://127.0.0.1:7077/mcp"

const (
	maxInputBytes   = 16 << 20
	maxStoredRunes  = 2000 // per captured prompt/response in the session log
	contextBudget   = 1500 // tokens requested from get_context
	maxContextRunes = 6000 // Claude Code caps hook context at 10,000 characters
	sessionMaxAge   = 14 * 24 * time.Hour
	spoolMaxAge     = 30 * 24 * time.Hour
	maxFlushPerRun  = 20
)

// Input is the subset of the hook payload Kenfold uses. Claude Code and Codex
// share these field names.
type Input struct {
	SessionID            string `json:"session_id"`
	HookEventName        string `json:"hook_event_name"`
	Cwd                  string `json:"cwd"`
	Source               string `json:"source"`                 // SessionStart
	Reason               string `json:"reason"`                 // SessionEnd
	Prompt               string `json:"prompt"`                 // UserPromptSubmit
	LastAssistantMessage string `json:"last_assistant_message"` // Stop
	CompactSummary       string `json:"compact_summary"`        // PostCompact (Claude Code)
}

// Options configures Run.
type Options struct {
	URL        string // MCP endpoint (default DefaultURL)
	APIKey     string // bearer token; optional when the server runs with KENFOLD_AUTH=none
	ClientName string // MCP clientInfo name; only used for attribution without API keys
	StateDir   string // local state (session logs, spool); required
	Capture    bool   // record sessions and write summaries
	Version    string

	HTTPClient *http.Client // tests
	Now        func() time.Time
	Stderr     io.Writer

	ContextTimeout time.Duration // get_context at SessionStart (default 4s)
	SendTimeout    time.Duration // remember at SessionEnd (default 1s: SessionEnd budgets are short)
	FlushBudget    time.Duration // sending spooled summaries at SessionStart (default 3s)
	RefsBudget     time.Duration // code reference sync at SessionStart (default 2s)
	NoRefs         bool          // skip code reference sync
}

func (o *Options) defaults() {
	if o.URL == "" {
		o.URL = DefaultURL
	}
	if o.ClientName == "" {
		o.ClientName = "kenfold-hook"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if o.ContextTimeout <= 0 {
		o.ContextTimeout = 4 * time.Second
	}
	if o.SendTimeout <= 0 {
		o.SendTimeout = time.Second
	}
	if o.FlushBudget <= 0 {
		o.FlushBudget = 3 * time.Second
	}
	if o.RefsBudget <= 0 {
		o.RefsBudget = 2 * time.Second
	}
}

// destination binds a queued write without persisting the bearer token. Include
// the client name even with a configured token: an AUTH=none server ignores the
// token and attributes writes by client name.
func (o Options) destination() string {
	identity, mode := o.APIKey, "bearer"
	if identity == "" {
		identity, mode = o.ClientName, "client"
	}
	b, _ := json.Marshal([4]string{o.URL, mode, identity, o.ClientName})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// Run handles one hook invocation. It returns an error only for problems the
// caller should report; the session is never blocked either way.
func Run(ctx context.Context, stdin io.Reader, stdout io.Writer, o Options) error {
	o.defaults()
	if o.StateDir == "" {
		return errors.New("no state directory")
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, maxInputBytes+1))
	if err != nil {
		return fmt.Errorf("read hook input: %w", err)
	}
	if len(raw) > maxInputBytes {
		return errors.New("hook input is too large")
	}
	var in Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("hook input is not JSON: %w", err)
	}
	r := &runner{o: o, in: in, st: state{dir: o.StateDir}, c: newClient(o)}
	defer r.c.close()

	switch in.HookEventName {
	case "SessionStart":
		return r.sessionStart(ctx, stdout)
	case "UserPromptSubmit":
		return r.capture(ctx, kindPrompt, in.Prompt)
	case "Stop":
		return r.capture(ctx, kindResponse, in.LastAssistantMessage)
	case "PostCompact":
		return r.capture(ctx, kindCompact, in.CompactSummary)
	case "SessionEnd":
		return r.sessionEnd(ctx)
	case "":
		return errors.New("hook input has no hook_event_name")
	default:
		return nil // not an event Kenfold handles
	}
}

type runner struct {
	o  Options
	in Input
	st state
	c  *client
}

func (r *runner) warn(format string, args ...any) {
	fmt.Fprintf(r.o.Stderr, "kenfold hook: "+format+"\n", args...)
}

// ---- SessionStart ----

type hookOutput struct {
	HookSpecificOutput *specific `json:"hookSpecificOutput,omitempty"`
	SystemMessage      string    `json:"systemMessage,omitempty"`
}

type specific struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

func (r *runner) sessionStart(ctx context.Context, stdout io.Writer) error {
	project, branch := detectProject(ctx, r.in.Cwd)
	if r.o.Capture && r.in.SessionID != "" && project != "" {
		if err := r.record(event{Time: r.o.Now(), Kind: kindStart, Project: project, Branch: branch}); err != nil {
			r.warn("record session start: %v", err)
		}
	}
	r.st.cleanup(r.o.Now())

	// Deliver summaries from sessions that ended while the server was down.
	flushCtx, cancel := context.WithTimeout(ctx, r.o.FlushBudget)
	r.flush(flushCtx)
	cancel()

	// Tell the server whether code that memories refer to changed, so the
	// context below can flag stale memories.
	if project != "" && !r.o.NoRefs {
		r.syncRefs(ctx, project)
	}

	cctx, cancel := context.WithTimeout(ctx, r.o.ContextTimeout)
	defer cancel()
	args := map[string]any{"budget_tokens": contextBudget}
	if project != "" {
		args["project"] = project
	}
	var out mcpserver.GetContextOutput
	if err := r.c.call(cctx, "get_context", args, r.in.SessionID, &out); err != nil {
		r.warn("get_context: %v", err)
		return writeJSON(stdout, hookOutput{SystemMessage: "Kenfold memory was not loaded: " + oneLine(err.Error(), 200)})
	}
	text := formatContext(project, out)
	if text == "" {
		return nil
	}
	return writeJSON(stdout, hookOutput{HookSpecificOutput: &specific{HookEventName: "SessionStart", AdditionalContext: text}})
}

// syncRefs checks the project's code references against the repository
// (see coderef.Sync) within RefsBudget. Expected conditions (no commits yet,
// an older server without the API, the server down, the budget spent) are
// silent; get_context reports an unreachable server.
func (r *runner) syncRefs(ctx context.Context, project string) {
	base, err := coderef.APIBase(r.o.URL)
	if err != nil {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, r.o.RefsBudget)
	defer cancel()
	_, err = coderef.Sync(sctx, coderef.SyncOptions{APIBase: base, APIKey: r.o.APIKey, Dir: r.in.Cwd, Project: project, HTTPClient: r.o.HTTPClient})
	var se *coderef.StatusError
	switch {
	case err == nil, errors.Is(err, coderef.ErrNoCommits), sctx.Err() != nil:
	case errors.As(err, &se) && (se.Code == http.StatusNotFound || se.Code == http.StatusMethodNotAllowed):
	case !errors.As(err, &se) && transient(err):
	default:
		r.warn("code reference sync: %v", err)
	}
}

// ---- capture ----

func (r *runner) capture(ctx context.Context, kind, text string) error {
	if !r.o.Capture || r.in.SessionID == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	project, _ := detectProject(ctx, r.in.Cwd)
	if project == "" {
		return nil
	}
	clean, _ := secrets.Redact(strings.TrimSpace(text))
	return r.record(event{Time: r.o.Now(), Kind: kind, Project: project, Text: truncate(clean, maxStoredRunes)})
}

// The first project-tagged event binds a session even without SessionStart.
// Moving to another repository must not add its text to that project's log.
func (r *runner) record(e event) error {
	events, err := r.st.read(r.in.SessionID)
	if err != nil {
		return err
	}
	if project := sessionProject(events); project != "" && project != e.Project {
		r.warn("skip capture from a different project than this session")
		return nil
	}
	return r.st.append(r.in.SessionID, e)
}

func sessionProject(events []event) string {
	for _, e := range events {
		if e.Project != "" {
			return e.Project
		}
	}
	return ""
}

// ---- SessionEnd ----

func (r *runner) sessionEnd(ctx context.Context) error {
	if !r.o.Capture || r.in.SessionID == "" {
		return nil
	}
	events, err := r.st.read(r.in.SessionID)
	if err != nil {
		return fmt.Errorf("read session log: %w", err)
	}
	project := sessionProject(events)
	if project == "" {
		return nil // never guess the provenance of an unbound legacy log from cwd
	}
	// Also filter at delivery, protecting old mixed-project logs or simultaneous
	// first captures. Legacy untagged events inherit only a preceding binding.
	var bound []event
	current := ""
	for _, e := range events {
		if e.Project != "" {
			current = e.Project
		}
		if current == project {
			bound = append(bound, e)
		}
	}
	summary := buildSummary(bound, r.o.Now())
	if summary == "" {
		return nil // nothing was asked in this session
	}
	p := pending{Project: project, Content: summary, Type: "episodic", SessionID: r.in.SessionID, CreatedAt: r.o.Now(), Destination: r.o.destination()}
	// Spool first: if the send below is cut short by the client's hook
	// timeout, the summary is delivered at the next SessionStart. The server
	// deduplicates, so a summary that did arrive is not stored twice.
	file, err := r.st.spool(p)
	if err != nil {
		return fmt.Errorf("spool summary: %w", err)
	}
	sctx, cancel := context.WithTimeout(ctx, r.o.SendTimeout)
	defer cancel()
	if err := r.send(sctx, p); err != nil {
		if transient(err) {
			return nil // stays spooled
		}
		r.warn("session summary rejected: %v", err)
	}
	r.st.unspool(file)
	return nil
}

func (r *runner) send(ctx context.Context, p pending) error {
	args := map[string]any{"content": p.Content, "type": p.Type, "project": p.Project}
	return r.c.call(ctx, "remember", args, p.SessionID, nil)
}

// flush sends spooled summaries, oldest first, until the budget runs out or
// the server looks unreachable.
func (r *runner) flush(ctx context.Context) {
	files := r.st.spooled()
	sent := 0
	for _, f := range files {
		if sent == maxFlushPerRun || ctx.Err() != nil {
			return
		}
		p, err := r.st.load(f)
		if err != nil {
			r.warn("drop unreadable spooled summary %s: %v", filepath.Base(f), err)
			r.st.unspool(f)
			continue
		}
		if p.Destination == "" {
			r.warn("keep unbound legacy summary %s: its original endpoint and identity are unknown", filepath.Base(f))
			continue
		}
		if p.Destination != r.o.destination() {
			continue
		}
		sent++
		if err := r.send(ctx, p); err != nil {
			if transient(err) {
				return
			}
			r.warn("drop spooled summary rejected by the server: %v", err)
		}
		r.st.unspool(f)
	}
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// DefaultStateDir returns $XDG_STATE_HOME/kenfold, or ~/.local/state/kenfold.
func DefaultStateDir(getenv func(string) string) (string, error) {
	if d := getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "kenfold"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "kenfold"), nil
}
