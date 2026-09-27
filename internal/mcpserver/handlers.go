package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

// handlers holds the dependencies shared by the implemented tool handlers.
type handlers struct {
	store *store.Store
}

// remember stores a durable memory. Type defaults to semantic when omitted
// (full server-side classification is Phase 2). Scope is derived from project:
// user-wide when project is empty, otherwise project:<normalized>. New
// preferences are stored as proposed pending user confirmation (see
// docs/mcp-tools.md); everything else is active.
func (h *handlers) remember(ctx context.Context, req *mcp.CallToolRequest, in RememberInput) (*mcp.CallToolResult, RememberOutput, error) {
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return toolError[RememberOutput]("content must not be empty")
	}

	typ := in.Type
	if typ == "" {
		typ = memory.TypeSemantic
	}

	p := store.CreateParams{
		Type:        typ,
		Scope:       scopeFor(in.Project),
		Content:     content,
		SourceAgent: agentName(req),
		Trust:       memory.TrustAgent,
		Confidence:  0.5,
		Status:      statusFor(typ),
	}

	if in.Supersedes != "" {
		p.Supersedes = &in.Supersedes
	}

	if typ == memory.TypeTemporary {
		if in.TTLSeconds <= 0 {
			return toolError[RememberOutput]("ttl_seconds is required and must be positive for type=temporary")
		}
	}
	if in.TTLSeconds > 0 {
		exp := time.Now().Add(time.Duration(in.TTLSeconds) * time.Second)
		p.ExpiresAt = &exp
	}

	m, err := h.store.Create(ctx, p)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return toolError[RememberOutput](fmt.Sprintf("supersedes: memory %q not found or not active", in.Supersedes))
		}
		return nil, RememberOutput{}, err
	}

	return nil, RememberOutput{ID: m.ID, Type: m.Type, Status: m.Status}, nil
}

// forget soft-deletes a memory. It is idempotent: forgetting a memory that is
// already deleted (or forgetting twice) returns status=deleted without error.
func (h *handlers) forget(ctx context.Context, _ *mcp.CallToolRequest, in ForgetInput) (*mcp.CallToolResult, ForgetOutput, error) {
	if strings.TrimSpace(in.ID) == "" {
		return toolError[ForgetOutput]("id must not be empty")
	}
	m, err := h.store.SoftDelete(ctx, in.ID, in.Reason)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return toolError[ForgetOutput](fmt.Sprintf("memory %q not found", in.ID))
		}
		return nil, ForgetOutput{}, err
	}
	return nil, ForgetOutput{ID: m.ID, Status: m.Status}, nil
}

// defaultHandoffTTL is used when the caller does not specify one.
const defaultHandoffTTL = 7 * 24 * time.Hour

// recall runs a lexical full-text search over shared memory. Vector/hybrid
// ranking is a later phase; this returns FTS matches ranked by ts_rank.
func (h *handlers) recall(ctx context.Context, _ *mcp.CallToolRequest, in RecallInput) (*mcp.CallToolResult, RecallOutput, error) {
	if strings.TrimSpace(in.Query) == "" {
		return toolError[RecallOutput]("query must not be empty")
	}
	scored, err := h.store.Search(ctx, store.SearchParams{
		Query: in.Query,
		Scope: scopeFor(in.Project),
		Types: in.Types,
		Limit: in.Limit,
	})
	if err != nil {
		return nil, RecallOutput{}, err
	}
	out := RecallOutput{Memories: make([]MemoryView, 0, len(scored))}
	for _, s := range scored {
		v := toMemoryView(s.Memory)
		v.Score = s.Score
		out.Memories = append(out.Memories, v)
	}
	return nil, out, nil
}

// handoff stores a note for the next agent/session as a temporary memory with a
// TTL. next_steps and the handoff marker are kept in attrs so resume can find it.
func (h *handlers) handoff(ctx context.Context, req *mcp.CallToolRequest, in HandoffInput) (*mcp.CallToolResult, HandoffOutput, error) {
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return toolError[HandoffOutput]("summary must not be empty")
	}

	ttl := defaultHandoffTTL
	if in.TTLSeconds > 0 {
		ttl = time.Duration(in.TTLSeconds) * time.Second
	}
	exp := time.Now().Add(ttl)

	attrs := map[string]any{"kind": "handoff"}
	if len(in.NextSteps) > 0 {
		steps := make([]any, len(in.NextSteps))
		for i, s := range in.NextSteps {
			steps[i] = s
		}
		attrs["next_steps"] = steps
	}

	m, err := h.store.Create(ctx, store.CreateParams{
		Type:        memory.TypeTemporary,
		Scope:       scopeFor(in.Project),
		Content:     summary,
		Attrs:       attrs,
		SourceAgent: agentName(req),
		Trust:       memory.TrustAgent,
		Confidence:  0.5,
		Status:      memory.StatusActive,
		ExpiresAt:   &exp,
	})
	if err != nil {
		return nil, HandoffOutput{}, err
	}
	expires := time.Now().Add(ttl)
	if m.ExpiresAt != nil {
		expires = *m.ExpiresAt
	}
	return nil, HandoffOutput{ID: m.ID, ExpiresAt: expires}, nil
}

// resume returns the latest unexpired handoff for the project, or an empty
// result (no handoff field) when there is none.
func (h *handlers) resume(ctx context.Context, _ *mcp.CallToolRequest, in ResumeInput) (*mcp.CallToolResult, ResumeOutput, error) {
	m, err := h.store.LatestHandoff(ctx, scopeFor(in.Project))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ResumeOutput{}, nil
	}
	if err != nil {
		return nil, ResumeOutput{}, err
	}
	v := toMemoryView(m)
	return nil, ResumeOutput{Handoff: &v}, nil
}

// getContext assembles the memory context for the start of a task: user
// preferences, the pending handoff, project knowledge, and task-relevant
// memories. Vector ranking is a later phase; relevant[] uses FTS on the task.
func (h *handlers) getContext(ctx context.Context, _ *mcp.CallToolRequest, in GetContextInput) (*mcp.CallToolResult, GetContextOutput, error) {
	projectScope := scopeFor(in.Project)
	scopes := []string{"user"}
	if projectScope != "user" {
		scopes = append(scopes, projectScope)
	}

	var out GetContextOutput

	// Preferences are always injected (user-wide).
	prefs, err := h.store.ListByScopeTypes(ctx, []string{"user"}, []memory.Type{memory.TypePreference}, 50)
	if err != nil {
		return nil, GetContextOutput{}, err
	}
	out.Preferences = toMemoryViews(prefs)

	// Pending handoff for this project, if any.
	if hm, err := h.store.LatestHandoff(ctx, projectScope); err == nil {
		v := toMemoryView(hm)
		out.Handoff = &v
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, GetContextOutput{}, err
	}

	// Project knowledge.
	if projectScope != "user" {
		proj, err := h.store.ListByScopeTypes(ctx, []string{projectScope}, []memory.Type{memory.TypeProject}, 50)
		if err != nil {
			return nil, GetContextOutput{}, err
		}
		out.Project = toMemoryViews(proj)
	}

	// Task-relevant memories via FTS on the task description.
	if strings.TrimSpace(in.Task) != "" {
		scored, err := h.store.Search(ctx, store.SearchParams{Query: in.Task, Scope: projectScope, Limit: 10})
		if err != nil {
			return nil, GetContextOutput{}, err
		}
		for _, s := range scored {
			v := toMemoryView(s.Memory)
			v.Score = s.Score
			out.Relevant = append(out.Relevant, v)
		}
	}

	return nil, out, nil
}

// toMemoryView maps a stored memory to the agent-facing view.
func toMemoryView(m store.Memory) MemoryView {
	return MemoryView{
		ID:          m.ID,
		Type:        m.Type,
		Scope:       m.Scope,
		Content:     m.Content,
		SourceAgent: m.SourceAgent,
		Trust:       m.Trust,
		CreatedAt:   m.CreatedAt,
	}
}

func toMemoryViews(ms []store.Memory) []MemoryView {
	if len(ms) == 0 {
		return nil
	}
	out := make([]MemoryView, len(ms))
	for i, m := range ms {
		out[i] = toMemoryView(m)
	}
	return out
}

// scopeFor maps a project argument to a scope. An empty project is user-wide.
func scopeFor(project string) string {
	project = strings.TrimSpace(project)
	if project == "" {
		return "user"
	}
	return "project:" + normalizeProject(project)
}

// normalizeProject reduces a git remote URL to a stable host/path identity so
// the same repository maps to the same project across agents and machines
// (see ADR-0001). Free-form names are returned trimmed.
func normalizeProject(p string) string {
	p = strings.TrimSpace(p)
	// scp-like syntax: git@github.com:owner/repo.git
	if strings.HasPrefix(p, "git@") {
		if _, rest, ok := strings.Cut(p, "@"); ok {
			p = strings.Replace(rest, ":", "/", 1)
		}
	}
	// strip scheme: https://, ssh://, git://
	if _, rest, ok := strings.Cut(p, "://"); ok {
		p = rest
	}
	// strip userinfo (user@host)
	if _, rest, ok := strings.Cut(p, "@"); ok && strings.Contains(p, "/") {
		p = rest
	}
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimSuffix(p, ".git")
	return p
}

// statusFor decides the initial lifecycle state. Preferences are held for user
// confirmation; other types are active immediately.
func statusFor(t memory.Type) memory.Status {
	if t == memory.TypePreference {
		return memory.StatusProposed
	}
	return memory.StatusActive
}

// agentName resolves the calling agent from MCP clientInfo. Phase 1 has no API
// keys yet, so clientInfo.name is the source; it falls back to "unknown".
func agentName(req *mcp.CallToolRequest) string {
	if req != nil && req.Session != nil {
		if p := req.Session.InitializeParams(); p != nil && p.ClientInfo != nil {
			if name := strings.TrimSpace(p.ClientInfo.Name); name != "" {
				return name
			}
		}
	}
	return "unknown"
}

// toolError returns a domain error the model can react to. The SDK wraps a
// non-nil handler error in a CallToolResult with IsError:true and the message
// in a text block, and skips output-schema validation (which a zero-valued
// output would otherwise fail). This is distinct from a protocol-level error.
func toolError[Out any](msg string) (*mcp.CallToolResult, Out, error) {
	var zero Out
	return nil, zero, errors.New(msg)
}
