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
