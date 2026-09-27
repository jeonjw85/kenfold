// Package memory defines Kenfold's core domain vocabulary. These values are
// mirrored by CHECK constraints in migrations/00001_init.sql; keep both in sync.
package memory

import "fmt"

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
