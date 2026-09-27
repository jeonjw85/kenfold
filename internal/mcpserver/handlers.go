package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenfold/kenfold/internal/apikey"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

// Limits on tool input. They keep memories focused and requests bounded.
const (
	maxContentRunes   = 8000
	maxQueryRunes     = 2000
	maxReasonRunes    = 1000
	maxNextSteps      = 20
	maxNextStepRunes  = 1000
	maxTTL            = 365 * 24 * time.Hour
	defaultHandoffTTL = 7 * 24 * time.Hour
	maxHandoffTTL     = 30 * 24 * time.Hour
	defaultBudget     = 2000
	minBudget         = 200
	maxBudget         = 32000
	contextListLimit  = 50
	contextRelevant   = 10
	// embedTimeout bounds a single embedding call. On timeout or error the
	// tool still succeeds using full-text search; writes are embedded later.
	embedTimeout = 10 * time.Second
)

// handlers holds the dependencies shared by the tool handlers.
type handlers struct {
	store       *store.Store
	embedder    store.Embedder
	log         *slog.Logger
	agent       string
	maxDistance float64
}

func newHandlers(d Deps) *handlers {
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &handlers{store: d.Store, embedder: d.Embedder, log: log, agent: d.Agent, maxDistance: d.MaxDistance}
}

// ---- remember ----

func (h *handlers) remember(ctx context.Context, req *mcp.CallToolRequest, in RememberInput) (*mcp.CallToolResult, RememberOutput, error) {
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return toolError[RememberOutput]("content must not be empty")
	}
	if n := utf8.RuneCountInString(content); n > maxContentRunes {
		return toolError[RememberOutput](fmt.Sprintf("content is %d characters; the limit is %d. Store one self-contained fact per memory", n, maxContentRunes))
	}
	scope, err := scopeFor(in.Project)
	if err != nil {
		return toolError[RememberOutput](err.Error())
	}
	ttl, err := ttlFrom(in.TTLSeconds, 0, maxTTL)
	if err != nil {
		return toolError[RememberOutput](err.Error())
	}
	typ := in.Type
	if typ == "" {
		typ = defaultType(scope)
	}
	if typ == memory.TypeTemporary && ttl == 0 {
		return toolError[RememberOutput]("ttl_seconds is required for type=temporary")
	}

	var supersedes *string
	if in.Supersedes != "" {
		id := strings.TrimSpace(in.Supersedes)
		if !store.ValidID(id) {
			return toolError[RememberOutput](fmt.Sprintf("supersedes: %q is not a memory id", in.Supersedes))
		}
		old, err := h.store.Get(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return toolError[RememberOutput](fmt.Sprintf("supersedes: memory %s not found", id))
		}
		if err != nil {
			return nil, RememberOutput{}, h.internal(ctx, "remember", err)
		}
		if old.Scope != scope {
			return toolError[RememberOutput](fmt.Sprintf("supersedes: memory %s is in scope %q, but this memory would be in %q; pass the same project", id, old.Scope, scope))
		}
		if old.Status != memory.StatusActive {
			return toolError[RememberOutput](fmt.Sprintf("supersedes: memory %s is %s, not active; use recall to find the current version", id, old.Status))
		}
		supersedes = &id
	} else {
		// Storing the same fact twice returns the existing memory.
		dup, err := h.store.FindDuplicate(ctx, scope, typ, content)
		if err == nil {
			return nil, RememberOutput{ID: dup.ID, Type: dup.Type, Status: dup.Status, Deduplicated: true}, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, RememberOutput{}, h.internal(ctx, "remember", err)
		}
	}

	p := store.CreateParams{
		Type:        typ,
		Scope:       scope,
		Content:     content,
		SourceAgent: h.agentOf(req),
		Trust:       memory.TrustAgent,
		Confidence:  0.5,
		Status:      statusFor(typ),
		Supersedes:  supersedes,
	}
	if ttl > 0 {
		exp := time.Now().Add(ttl)
		p.ExpiresAt = &exp
	}
	if vec := h.embed(ctx, "remember", content); vec != nil {
		p.Embedding, p.EmbeddingModel = vec, h.embedder.Model()
	}

	m, err := h.store.Create(ctx, p)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrNotActive):
		// The superseded memory changed between the check above and the insert.
		return toolError[RememberOutput](fmt.Sprintf("supersedes: memory %s is no longer active; use recall to find the current version", in.Supersedes))
	case err != nil:
		return nil, RememberOutput{}, h.internal(ctx, "remember", err)
	}
	return nil, RememberOutput{ID: m.ID, Type: m.Type, Status: m.Status}, nil
}

// ---- forget ----

func (h *handlers) forget(ctx context.Context, req *mcp.CallToolRequest, in ForgetInput) (*mcp.CallToolResult, ForgetOutput, error) {
	id := strings.TrimSpace(in.ID)
	if !store.ValidID(id) {
		return toolError[ForgetOutput](fmt.Sprintf("%q is not a memory id (use an id returned by remember, recall, or get_context)", in.ID))
	}
	reason := truncateRunes(strings.TrimSpace(in.Reason), maxReasonRunes)
	m, err := h.store.SoftDelete(ctx, id, reason, h.agentOf(req))
	if errors.Is(err, store.ErrNotFound) {
		return toolError[ForgetOutput](fmt.Sprintf("memory %s not found", id))
	}
	if err != nil {
		return nil, ForgetOutput{}, h.internal(ctx, "forget", err)
	}
	return nil, ForgetOutput{ID: m.ID, Status: m.Status}, nil
}

// ---- recall ----

func (h *handlers) recall(ctx context.Context, _ *mcp.CallToolRequest, in RecallInput) (*mcp.CallToolResult, RecallOutput, error) {
	query := truncateRunes(strings.TrimSpace(in.Query), maxQueryRunes)
	if query == "" {
		return toolError[RecallOutput]("query must not be empty")
	}
	scope, err := scopeFor(in.Project)
	if err != nil {
		return toolError[RecallOutput](err.Error())
	}
	res, err := h.search(ctx, "recall", query, scope, in.Types, in.Limit)
	if err != nil {
		return nil, RecallOutput{}, h.internal(ctx, "recall", err)
	}
	out := RecallOutput{Memories: make([]MemoryView, 0, len(res))}
	for _, r := range res {
		v := toMemoryView(r.Memory)
		v.Score = r.Score
		out.Memories = append(out.Memories, v)
	}
	return nil, out, nil
}

// ---- handoff / resume ----

func (h *handlers) handoff(ctx context.Context, req *mcp.CallToolRequest, in HandoffInput) (*mcp.CallToolResult, HandoffOutput, error) {
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return toolError[HandoffOutput]("summary must not be empty")
	}
	if n := utf8.RuneCountInString(summary); n > maxContentRunes {
		return toolError[HandoffOutput](fmt.Sprintf("summary is %d characters; the limit is %d", n, maxContentRunes))
	}
	var steps []string
	for _, s := range in.NextSteps {
		if s = strings.TrimSpace(s); s != "" {
			if utf8.RuneCountInString(s) > maxNextStepRunes {
				return toolError[HandoffOutput](fmt.Sprintf("next_steps: each step is limited to %d characters", maxNextStepRunes))
			}
			steps = append(steps, s)
		}
	}
	if len(steps) > maxNextSteps {
		return toolError[HandoffOutput](fmt.Sprintf("next_steps: at most %d steps", maxNextSteps))
	}
	scope, err := scopeFor(in.Project)
	if err != nil {
		return toolError[HandoffOutput](err.Error())
	}
	ttl, err := ttlFrom(in.TTLSeconds, defaultHandoffTTL, maxHandoffTTL)
	if err != nil {
		return toolError[HandoffOutput](err.Error())
	}

	m, err := h.store.CreateHandoff(ctx, store.HandoffParams{
		Scope:       scope,
		Summary:     summary,
		NextSteps:   steps,
		SourceAgent: h.agentOf(req),
		ExpiresAt:   time.Now().Add(ttl),
	})
	if err != nil {
		return nil, HandoffOutput{}, h.internal(ctx, "handoff", err)
	}
	if m.ExpiresAt == nil { // cannot happen: handoffs always have a TTL
		return nil, HandoffOutput{}, h.internal(ctx, "handoff", errors.New("stored handoff has no expiry"))
	}
	return nil, HandoffOutput{ID: m.ID, ExpiresAt: *m.ExpiresAt}, nil
}

func (h *handlers) resume(ctx context.Context, req *mcp.CallToolRequest, in ResumeInput) (*mcp.CallToolResult, ResumeOutput, error) {
	scope, err := scopeFor(in.Project)
	if err != nil {
		return toolError[ResumeOutput](err.Error())
	}
	m, err := h.store.LatestHandoff(ctx, scope, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ResumeOutput{}, nil
	}
	if err != nil {
		return nil, ResumeOutput{}, h.internal(ctx, "resume", err)
	}
	if marked, err := h.store.MarkResumed(ctx, m.ID, h.agentOf(req)); err != nil {
		// The handoff is still useful to the caller; only the bookkeeping failed.
		h.log.WarnContext(ctx, "resume: could not mark handoff resumed", "id", m.ID, "err", err)
	} else {
		m = marked
	}
	v := toMemoryView(m)
	return nil, ResumeOutput{Handoff: &v}, nil
}

// ---- get_context ----

// getContext assembles, in priority order and within an approximate token
// budget: preferences, the pending (not yet resumed) handoff, project
// knowledge, and memories relevant to the task. A memory appears at most once.
func (h *handlers) getContext(ctx context.Context, _ *mcp.CallToolRequest, in GetContextInput) (*mcp.CallToolResult, GetContextOutput, error) {
	scope, err := scopeFor(in.Project)
	if err != nil {
		return toolError[GetContextOutput](err.Error())
	}
	if in.BudgetTokens < 0 {
		return toolError[GetContextOutput]("budget_tokens must be positive")
	}
	b := budget{left: defaultBudget, seen: map[string]bool{}}
	if in.BudgetTokens > 0 {
		b.left = min(max(in.BudgetTokens, minBudget), maxBudget)
	}
	scopes := []string{"user"}
	if scope != "user" {
		scopes = append(scopes, scope)
	}
	var out GetContextOutput

	prefs, err := h.store.List(ctx, store.ListParams{Scopes: scopes, Types: []memory.Type{memory.TypePreference}, Limit: contextListLimit})
	if err != nil {
		return nil, GetContextOutput{}, h.internal(ctx, "get_context", err)
	}
	for _, m := range prefs {
		if v, ok := b.take(toMemoryView(m)); ok {
			out.Preferences = append(out.Preferences, v)
		}
	}

	switch hm, err := h.store.LatestHandoff(ctx, scope, true); {
	case err == nil:
		if v, ok := b.take(toMemoryView(hm)); ok {
			out.Handoff = &v
		}
	case !errors.Is(err, store.ErrNotFound):
		return nil, GetContextOutput{}, h.internal(ctx, "get_context", err)
	}

	if scope != "user" {
		proj, err := h.store.List(ctx, store.ListParams{Scopes: []string{scope}, Types: []memory.Type{memory.TypeProject}, Limit: contextListLimit})
		if err != nil {
			return nil, GetContextOutput{}, h.internal(ctx, "get_context", err)
		}
		for _, m := range proj {
			if v, ok := b.take(toMemoryView(m)); ok {
				out.Project = append(out.Project, v)
			}
		}
	}

	if task := truncateRunes(strings.TrimSpace(in.Task), maxQueryRunes); task != "" {
		res, err := h.search(ctx, "get_context", task, scope, nil, contextRelevant)
		if err != nil {
			return nil, GetContextOutput{}, h.internal(ctx, "get_context", err)
		}
		for _, r := range res {
			v := toMemoryView(r.Memory)
			v.Score = r.Score
			if v, ok := b.take(v); ok {
				out.Relevant = append(out.Relevant, v)
			}
		}
	}
	out.Truncated = b.truncated
	return nil, out, nil
}

// budget tracks the approximate token budget of get_context.
type budget struct {
	left      int
	seen      map[string]bool
	truncated bool
}

// take reports whether v fits (and reserves it). Duplicates are skipped
// silently; memories that do not fit mark the result as truncated.
func (b *budget) take(v MemoryView) (MemoryView, bool) {
	if b.seen[v.ID] {
		return v, false
	}
	cost := estimateTokens(v)
	if cost > b.left {
		b.truncated = true
		return v, false
	}
	b.left -= cost
	b.seen[v.ID] = true
	return v, true
}

// estimateTokens approximates the tokens v adds to the model's context. It
// errs on the high side for English (~4 bytes/token) and is close for Korean
// (~3 bytes per character), plus a fixed cost for the JSON fields.
func estimateTokens(v MemoryView) int {
	n := len(v.Content) + len(v.Scope)
	for _, s := range v.NextSteps {
		n += len(s) + 4
	}
	return n/3 + 40
}

// ---- shared helpers ----

// search runs a hybrid search, embedding the query when an embedder is set.
func (h *handlers) search(ctx context.Context, op, query, scope string, types []memory.Type, limit int) ([]store.Scored, error) {
	p := store.SearchParams{Query: query, Scope: scope, Types: types, Limit: limit, MaxDistance: h.maxDistance}
	if vec := h.embed(ctx, op, query); vec != nil {
		p.Vector, p.Model = vec, h.embedder.Model()
	}
	return h.store.Search(ctx, p)
}

// embed returns the embedding of text, or nil when embeddings are disabled or
// the provider fails. Failures are logged, never returned: Kenfold degrades to
// full-text search, and unembedded memories are filled in by the backfill.
func (h *handlers) embed(ctx context.Context, op, text string) []float32 {
	if h.embedder == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	vecs, err := h.embedder.Embed(ctx, []string{text})
	if err == nil && len(vecs) != 1 {
		err = fmt.Errorf("embedder returned %d vectors for 1 input", len(vecs))
	}
	if err != nil {
		h.log.WarnContext(ctx, "embedding failed; using full-text search only", "tool", op, "err", err)
		return nil
	}
	return vecs[0]
}

// agentOf identifies the caller: the API key's agent (authoritative), then
// the server's configured agent, then the MCP client's self-reported name.
func (h *handlers) agentOf(req *mcp.CallToolRequest) string {
	if req != nil && req.Extra != nil {
		if a, ok := apikey.AgentFrom(req.Extra.TokenInfo); ok {
			return a
		}
	}
	if h.agent != "" {
		return h.agent
	}
	if req != nil && req.Session != nil {
		if p := req.Session.InitializeParams(); p != nil && p.ClientInfo != nil {
			return memory.SanitizeAgent(p.ClientInfo.Name)
		}
	}
	return memory.UnknownAgent
}

// internal logs err and returns a generic tool error, so database details are
// not exposed to agents. Cancellation is passed through unlogged.
func (h *handlers) internal(ctx context.Context, tool string, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	h.log.ErrorContext(ctx, "tool failed", "tool", tool, "err", err)
	return fmt.Errorf("%s failed because of an internal error; the Kenfold server log has details", tool)
}

// toMemoryView maps a stored memory to the agent-facing view.
func toMemoryView(m store.Memory) MemoryView {
	v := MemoryView{
		ID:          m.ID,
		Type:        m.Type,
		Scope:       m.Scope,
		Content:     m.Content,
		SourceAgent: m.SourceAgent,
		Trust:       m.Trust,
		CreatedAt:   m.CreatedAt,
		ExpiresAt:   m.ExpiresAt,
	}
	if m.Attrs["kind"] == "handoff" {
		if steps, ok := m.Attrs["next_steps"].([]any); ok {
			for _, s := range steps {
				if str, ok := s.(string); ok {
					v.NextSteps = append(v.NextSteps, str)
				}
			}
		}
		if by, ok := m.Attrs["resumed_by"].(string); ok {
			v.ResumedBy = by
		}
	}
	return v
}

// scopeFor maps a project argument to a scope (see memory.Scope).
func scopeFor(project string) (string, error) { return memory.Scope(project) }

// defaultType classifies a memory whose type was omitted (Phase 1 heuristic;
// model-based classification is Phase 2).
func defaultType(scope string) memory.Type {
	if scope == "user" {
		return memory.TypeSemantic
	}
	return memory.TypeProject
}

// statusFor decides the initial lifecycle state. Preferences require user
// confirmation (ADR-0001), so agents can only propose them.
func statusFor(t memory.Type) memory.Status {
	if t == memory.TypePreference {
		return memory.StatusProposed
	}
	return memory.StatusActive
}

// ttlFrom converts a ttl_seconds argument: 0 means def; negative or above
// maxTTL is an error. It avoids overflow for very large inputs.
func ttlFrom(seconds int, def, maxTTL time.Duration) (time.Duration, error) {
	switch {
	case seconds < 0:
		return 0, errors.New("ttl_seconds must be positive")
	case seconds == 0:
		return def, nil
	case int64(seconds) > int64(maxTTL/time.Second):
		return 0, fmt.Errorf("ttl_seconds must be at most %d (%s)", int64(maxTTL/time.Second), humanDuration(maxTTL))
	}
	return time.Duration(seconds) * time.Second, nil
}

func humanDuration(d time.Duration) string {
	if days := d / (24 * time.Hour); days > 0 && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", days)
	}
	return d.String()
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// toolError returns a domain error the model can react to. The SDK wraps a
// non-nil handler error in a result with isError:true and the message as text,
// and skips output-schema validation (a zero output would otherwise fail it).
func toolError[Out any](msg string) (*mcp.CallToolResult, Out, error) {
	var zero Out
	return nil, zero, errors.New(msg)
}
