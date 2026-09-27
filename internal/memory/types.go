// Package memory defines Kenfold's core domain vocabulary. These values are
// mirrored by CHECK constraints in migrations/; keep both in sync.
package memory

import (
	"fmt"
	"regexp"
	"strings"
)

// Type is the kind of a memory. Each type has its own write rules, lifetime,
// and retrieval strategy (see docs/adr/0001-architecture.md).
type Type string

const (
	TypeSemantic   Type = "semantic"   // durable facts and knowledge
	TypeEpisodic   Type = "episodic"   // what happened, when, in which session
	TypeProject    Type = "project"    // decisions, conventions, status of a project
	TypePreference Type = "preference" // user preferences; always injected, small
	TypeCodebase   Type = "codebase"   // code-derived knowledge, invalidated by commits
	TypeTemporary  Type = "temporary"  // short-lived context (handoffs, WIP); has a TTL
)

// Types lists every valid Type in a stable order.
var Types = []Type{TypeSemantic, TypeEpisodic, TypeProject, TypePreference, TypeCodebase, TypeTemporary}

// Valid reports whether t is a known memory type.
func (t Type) Valid() bool {
	for _, v := range Types {
		if t == v {
			return true
		}
	}
	return false
}

// ParseType converts s to a Type, rejecting unknown values.
func ParseType(s string) (Type, error) {
	t := Type(s)
	if !t.Valid() {
		return "", fmt.Errorf("unknown memory type %q", s)
	}
	return t, nil
}

// Trust records how much a memory's origin can be trusted. It is the basis for
// memory-poisoning defenses: external content is never promoted automatically.
type Trust string

const (
	TrustUser     Trust = "user"     // stated or confirmed by the user
	TrustAgent    Trust = "agent"    // produced by an agent from its own reasoning
	TrustExternal Trust = "external" // derived from external content (web, files, tool output)
)

// Status is the lifecycle state of a memory.
type Status string

const (
	StatusProposed   Status = "proposed"   // awaiting review, not served by default
	StatusActive     Status = "active"     // served in recall and get_context
	StatusSuperseded Status = "superseded" // replaced by a newer memory; kept for history
	StatusDeleted    Status = "deleted"    // soft-deleted via forget
)

// Agent names identify the agent that wrote a memory (memory.source_agent) and
// own API keys (api_key.agent). The pattern mirrors the api_key CHECK
// constraint in migrations/00002_api_keys_embedding_model.sql.
var agentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// UnknownAgent is recorded when the caller cannot be identified.
const UnknownAgent = "unknown"

// ValidAgent reports whether name is a well-formed agent name, e.g.
// "claude-code", "codex", "opencode".
func ValidAgent(name string) bool { return agentPattern.MatchString(name) }

// SanitizeAgent turns a free-form client name (such as MCP clientInfo.name)
// into a valid agent name: lowercased, invalid characters replaced by '-',
// truncated to 64 characters. It returns UnknownAgent if nothing usable remains.
func SanitizeAgent(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.TrimLeft(b.String(), "._-")
	if len(s) > 64 {
		s = s[:64]
	}
	if !ValidAgent(s) {
		return UnknownAgent
	}
	return s
}
