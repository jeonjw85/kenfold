package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenfold/kenfold/internal/apikey"
	"github.com/kenfold/kenfold/internal/authz"
	"github.com/kenfold/kenfold/internal/coderef"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/retrieve"
	"github.com/kenfold/kenfold/internal/secrets"
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
	contextRecent     = 3
	recentRunes       = 800
	maxSessionIDRunes = 128
	// MetaSessionID is the _meta key a client (e.g. `kenfold hook`) uses to
	// tag writes with its agent session, recorded as memory.source_session.
	MetaSessionID = "kenfold/session_id"
	// embedTimeout bounds a single embedding call. On timeout or error the
	// tool still succeeds using full-text search; writes are embedded later.
	embedTimeout = 10 * time.Second
)

// handlers holds the dependencies shared by the tool handlers.
type handlers struct {
	store      *store.Store
	embedder   store.Embedder
	classifier Classifier
	retriever  *retrieve.Retriever
	log        *slog.Logger
	agent      string
}

func newHandlers(d Deps) *handlers {
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &handlers{
		store: d.Store, embedder: d.Embedder, classifier: d.Classifier, log: log, agent: d.Agent,
		retriever: &retrieve.Retriever{Store: d.Store, Embedder: d.Embedder, Reranker: d.Reranker, Logger: log,
			Options: retrieve.Options{MaxDistance: d.MaxDistance}},
	}
}

// classifyTimeout bounds type classification on remember. A small local
// model answers in a few seconds; past this, the scope-based default is used.
const classifyTimeout = 15 * time.Second

// classify picks a type for a memory stored without one: the classifier's
// answer when configured and it succeeds, otherwise the scope-based default.
// It returns the type and where it came from ("model" or "default").
func (h *handlers) classify(ctx context.Context, content, scope string) (memory.Type, string) {
	if h.classifier == nil {
		return defaultType(scope), "default"
	}
	cctx, cancel := context.WithTimeout(ctx, classifyTimeout)
	defer cancel()
	typ, conf, err := h.classifier.Classify(cctx, content, scope != memory.ScopeUser)
	if err != nil {
		h.log.WarnContext(ctx, "type classification failed; using the default type", "err", err)
		return defaultType(scope), "default"
	}
	h.log.DebugContext(ctx, "classified memory", "type", typ, "confidence", conf)
	return typ, "model"
}

// ---- remember ----

func (h *handlers) remember(ctx context.Context, req *mcp.CallToolRequest, in RememberInput) (*mcp.CallToolResult, RememberOutput, error) {
	if res, out, err, denied := denyReadOnly[RememberOutput](req, "remember"); denied {
		return res, out, err
	}
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
	if in.Type == memory.TypeTemporary && ttl == 0 {
		return toolError[RememberOutput]("ttl_seconds is required for type=temporary")
	}
	// Secrets are checked before anything else sees the content (including a
	// classification model).
	if fs := secrets.Scan(content); len(fs) > 0 {
		return h.rejectSecret(ctx, req, "remember", "content", fs)
	}
	agent, session := h.agentOf(req), sessionOf(req)

	typ := in.Type
	typeSource := "agent"
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
		if typ == "" {
			// A replacement is, by default, the same kind of memory.
			typ, typeSource = old.Type, "superseded"
		}
		if old.Type == typ && store.ContentKey(old.Content) == store.ContentKey(content) {
			// Replacing a memory with the same statement changes nothing.
			return nil, RememberOutput{ID: old.ID, Type: old.Type, Status: old.Status, Deduplicated: true}, nil
		}
		supersedes = &id
	} else {
		// Storing the same fact twice returns the existing memory. Without a
		// type, a duplicate of any type counts (and saves a classification call).
		dup, err := h.store.FindDuplicate(ctx, scope, typ, content)
		if err == nil {
			return nil, RememberOutput{ID: dup.ID, Type: dup.Type, Status: dup.Status, Deduplicated: true}, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, RememberOutput{}, h.internal(ctx, "remember", err)
		}
		if typ == "" {
			typ, typeSource = h.classify(ctx, content, scope)
		}
		// A session keeps one summary: a later summary from the same agent
		// session replaces the earlier one (sessions can end more than once,
		// e.g. after /clear or a resume).
		if typ == memory.TypeEpisodic && session != "" {
			prev, err := h.store.FindSessionMemory(ctx, scope, typ, agent, session)
			switch {
			case err == nil:
				supersedes = &prev.ID
			case !errors.Is(err, store.ErrNotFound):
				return nil, RememberOutput{}, h.internal(ctx, "remember", err)
			}
		}
	}
	if typ == memory.TypeTemporary && ttl == 0 {
		return toolError[RememberOutput]("ttl_seconds is required for type=temporary")
	}

	p := store.CreateParams{
		Type:          typ,
		Attrs:         map[string]any{"type_source": typeSource},
		Scope:         scope,
		Content:       content,
		SourceAgent:   agent,
		SourceSession: session,
		Trust:         memory.TrustAgent,
		Confidence:    0.5,
		Status:        statusFor(typ),
		Supersedes:    supersedes,
	}
	if ttl > 0 {
		exp := time.Now().Add(ttl)
		p.ExpiresAt = &exp
	}
	if vec := h.embed(ctx, "remember", content); vec != nil {
		p.Embedding, p.EmbeddingModel = vec, h.embedder.Model()
	}
	for _, r := range coderef.For(typ, scope, content) {
		p.Refs = append(p.Refs, store.RefTarget(r))
	}

	m, err := h.store.Create(ctx, p)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrNotActive):
		// The superseded memory changed between the check above and the insert.
		return toolError[RememberOutput](fmt.Sprintf("supersedes: memory %s is no longer active; use recall to find the current version", *supersedes))
	case err != nil:
		return nil, RememberOutput{}, h.internal(ctx, "remember", err)
	}
	out := RememberOutput{ID: m.ID, Type: m.Type, Status: m.Status}
	out.Similar = h.similar(ctx, m, p.Embedding, p.EmbeddingModel)
	return nil, out, nil
}

// similar finds existing memories close to m, so the agent can supersede or
// forget one it contradicts. It is best-effort: failures are logged and yield
// no hints. Session summaries and temporary notes are not checked; they are
// expected to resemble each other.
func (h *handlers) similar(ctx context.Context, m store.Memory, vec []float32, model string) []MemoryView {
	if m.Type == memory.TypeEpisodic || m.Type == memory.TypeTemporary {
		return nil
	}
	res, err := h.store.Similar(ctx, store.SimilarParams{
		Scope: m.Scope, Content: m.Content, Exclude: m.ID, Vector: vec, Model: model,
	})
	if err != nil {
		h.log.WarnContext(ctx, "similar memories lookup failed", "err", err)
		return nil
	}
	var out []MemoryView
	for _, r := range res {
		v := toMemoryView(r.Memory)
		v.Score = r.Score
		out = append(out, v)
	}
	h.attachRefs(ctx, out)
	return out
}

// rejectSecret refuses a write that contains credentials. The message names
// the kind of secret but never echoes the value; the log records rule ids only.
func (h *handlers) rejectSecret(ctx context.Context, req *mcp.CallToolRequest, tool, field string, fs []secrets.Finding) (*mcp.CallToolResult, RememberOutput, error) {
	rules := make([]string, len(fs))
	for i, f := range fs {
		rules[i] = f.Rule
	}
	h.log.WarnContext(ctx, "rejected write containing a secret", "tool", tool, "agent", h.agentOf(req), "rules", rules)
	return toolError[RememberOutput](secretMessage(field, fs))
}

func secretMessage(field string, fs []secrets.Finding) string {
	return fmt.Sprintf("%s appears to contain a %s. Credentials must never be stored in shared memory: remove the value "+
		"(you can say where it is kept, for example the name of the environment variable or secret manager entry) and try again",
		field, strings.Join(secrets.Labels(fs), ", "))
}

// ---- forget ----

func (h *handlers) forget(ctx context.Context, req *mcp.CallToolRequest, in ForgetInput) (*mcp.CallToolResult, ForgetOutput, error) {
	if res, out, err, denied := denyReadOnly[ForgetOutput](req, "forget"); denied {
		return res, out, err
	}
	id := strings.TrimSpace(in.ID)
	if !store.ValidID(id) {
		return toolError[ForgetOutput](fmt.Sprintf("%q is not a memory id (use an id returned by remember, recall, or get_context)", in.ID))
	}
	// forget must always succeed, so a secret in the reason is redacted rather
	// than rejected.
	reason, _ := secrets.Redact(truncateRunes(strings.TrimSpace(in.Reason), maxReasonRunes))
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
	// Queries are not stored, but they are sent to the embedding provider.
	query, _ = secrets.Redact(query)
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
	h.attachRefs(ctx, out.Memories)
	return nil, out, nil
}

// ---- handoff / resume ----

func (h *handlers) handoff(ctx context.Context, req *mcp.CallToolRequest, in HandoffInput) (*mcp.CallToolResult, HandoffOutput, error) {
	if res, out, err, denied := denyReadOnly[HandoffOutput](req, "handoff"); denied {
		return res, out, err
	}
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
	if fs := secrets.Scan(summary); len(fs) > 0 {
		_, _, err := h.rejectSecret(ctx, req, "handoff", "summary", fs)
		return nil, HandoffOutput{}, err
	}
	for i, s := range steps {
		if fs := secrets.Scan(s); len(fs) > 0 {
			_, _, err := h.rejectSecret(ctx, req, "handoff", fmt.Sprintf("next_steps[%d]", i), fs)
			return nil, HandoffOutput{}, err
		}
	}

	m, err := h.store.CreateHandoff(ctx, store.HandoffParams{
		Scope:         scope,
		Summary:       summary,
		NextSteps:     steps,
		SourceAgent:   h.agentOf(req),
		SourceSession: sessionOf(req),
		ExpiresAt:     time.Now().Add(ttl),
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
	if res, out, err, denied := denyReadOnly[ResumeOutput](req, "resume"); denied {
		return res, out, err
	}
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
// budget: preferences, the pending (not yet resumed) handoff, recent session
// summaries, project knowledge, and memories relevant to the task. A memory
// appears at most once.
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

	// What happened in the latest sessions here (summaries written by hooks).
	recent, err := h.store.RecentEpisodes(ctx, scope, contextRecent)
	if err != nil {
		return nil, GetContextOutput{}, h.internal(ctx, "get_context", err)
	}
	for _, m := range recent {
		v := toMemoryView(m)
		v.Content = truncateWithEllipsis(v.Content, recentRunes)
		if v, ok := b.take(v); ok {
			out.Recent = append(out.Recent, v)
		}
	}

	if scope != "user" {
		proj, err := h.store.List(ctx, store.ListParams{Scopes: []string{scope}, Types: []memory.Type{memory.TypeProject}, Limit: contextListLimit})
		if err != nil {
			return nil, GetContextOutput{}, h.internal(ctx, "get_context", err)
		}
		views := make([]MemoryView, len(proj))
		for i, m := range proj {
			views[i] = toMemoryView(m)
		}
		// Rules whose code is gone or changed come last, so they are the
		// first to be left out when the budget is tight.
		h.attachRefs(ctx, views)
		slices.SortStableFunc(views, func(a, b MemoryView) int {
			switch {
			case a.Stale == b.Stale:
				return 0
			case b.Stale:
				return -1
			}
			return 1
		})
		for _, v := range views {
			if v, ok := b.take(v); ok {
				out.Project = append(out.Project, v)
			}
		}
	}

	if task := truncateRunes(strings.TrimSpace(in.Task), maxQueryRunes); task != "" {
		task, _ = secrets.Redact(task)
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
		h.attachRefs(ctx, out.Relevant)
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

// search ranks memories for query (see package retrieve).
func (h *handlers) search(ctx context.Context, _, query, scope string, types []memory.Type, limit int) ([]store.Scored, error) {
	return h.retriever.Search(ctx, retrieve.Query{Text: query, Scope: scope, Types: types, Limit: limit})
}

// attachRefs fills in the code references of views (best-effort: on failure
// the views are returned without them).
func (h *handlers) attachRefs(ctx context.Context, views []MemoryView) {
	if len(views) == 0 {
		return
	}
	ids := make([]string, len(views))
	for i, v := range views {
		ids[i] = v.ID
	}
	refs, err := h.store.Refs(ctx, ids)
	if err != nil {
		h.log.WarnContext(ctx, "code references lookup failed", "err", err)
		return
	}
	for i := range views {
		for _, r := range refs[views[i].ID] {
			cv := CodeRefView{Path: r.Path, Symbol: r.Symbol, State: r.State}
			if cv.Path == "" && r.ResolvedPath != nil {
				cv.Path = *r.ResolvedPath
			}
			if r.CheckedCommit != nil {
				cv.CheckedCommit = shortCommit(*r.CheckedCommit)
			}
			views[i].CodeRefs = append(views[i].CodeRefs, cv)
			views[i].Stale = views[i].Stale || r.Stale()
		}
	}
}

func shortCommit(c string) string { return c[:min(len(c), 12)] }

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

// denyReadOnly returns a tool error when the caller's grant is read-only.
func denyReadOnly[Out any](req *mcp.CallToolRequest, tool string) (*mcp.CallToolResult, Out, error, bool) {
	var ti *auth.TokenInfo
	if req != nil && req.Extra != nil {
		ti = req.Extra.TokenInfo
	}
	if authz.CanWrite(ti) {
		var zero Out
		return nil, zero, nil, false
	}
	res, out, err := toolError[Out](tool + " is not allowed: this client was granted read-only access to Kenfold memory (the owner can connect it again with write access)")
	return res, out, err, true
}

// sessionOf returns the client's agent session id from _meta (see
// MetaSessionID), or "" if absent or malformed.
func sessionOf(req *mcp.CallToolRequest) string {
	if req == nil || req.Params == nil {
		return ""
	}
	s, _ := req.Params.Meta[MetaSessionID].(string)
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > maxSessionIDRunes || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ""
	}
	return s
}

func truncateWithEllipsis(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimRightFunc(truncateRunes(s, n-1), unicode.IsSpace) + "…"
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
