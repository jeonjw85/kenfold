package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kenfold/kenfold/internal/apikey"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

// These cover handler branches that return before touching the store, so they
// need no database. Store-backed paths are covered by the E2E test.

func TestValidationErrors(t *testing.T) {
	h := newHandlers(Deps{})
	ctx := context.Background()
	long := strings.Repeat("x", maxContentRunes+1)
	cases := map[string]error{}
	add := func(name string, err error) { cases[name] = err }

	_, _, err := h.remember(ctx, nil, RememberInput{Content: "  "})
	add("remember empty", err)
	_, _, err = h.remember(ctx, nil, RememberInput{Content: long})
	add("remember too long", err)
	_, _, err = h.remember(ctx, nil, RememberInput{Content: "wip", Type: memory.TypeTemporary})
	add("temporary without ttl", err)
	_, _, err = h.remember(ctx, nil, RememberInput{Content: "x", TTLSeconds: -1})
	add("negative ttl", err)
	_, _, err = h.remember(ctx, nil, RememberInput{Content: "x", TTLSeconds: 1 << 40})
	add("huge ttl", err)
	_, _, err = h.remember(ctx, nil, RememberInput{Content: "x", Project: "https://"})
	add("empty project", err)
	_, _, err = h.remember(ctx, nil, RememberInput{Content: "x", Supersedes: "not-an-id"})
	add("bad supersedes", err)
	_, _, err = h.forget(ctx, nil, ForgetInput{ID: " "})
	add("forget empty", err)
	_, _, err = h.forget(ctx, nil, ForgetInput{ID: "1; DROP TABLE memory"})
	add("forget non-uuid", err)
	_, _, err = h.recall(ctx, nil, RecallInput{Query: " "})
	add("recall empty", err)
	_, _, err = h.handoff(ctx, nil, HandoffInput{Summary: ""})
	add("handoff empty", err)
	_, _, err = h.handoff(ctx, nil, HandoffInput{Summary: "s", NextSteps: make21()})
	add("too many steps", err)
	_, _, err = h.handoff(ctx, nil, HandoffInput{Summary: "s", TTLSeconds: 31 * 24 * 3600})
	add("handoff ttl too long", err)
	_, _, err = h.getContext(ctx, nil, GetContextInput{BudgetTokens: -1})
	add("negative budget", err)
	_, _, err = h.resume(ctx, nil, ResumeInput{Project: "\x00bad"})
	add("control chars in project", err)

	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: no tool error", name)
		} else if strings.Contains(err.Error(), "internal error") {
			t.Errorf("%s: got internal error %q; want a validation message", name, err)
		}
	}
}

func make21() []string {
	s := make([]string, maxNextSteps+1)
	for i := range s {
		s[i] = "step"
	}
	return s
}

func TestScopeFor(t *testing.T) {
	// Normalization is tested in the memory package; check the delegation.
	if got, err := scopeFor("git@github.com:O/R.git"); err != nil || got != "project:github.com/o/r" {
		t.Errorf("scopeFor = %q, %v", got, err)
	}
	if got, err := scopeFor(""); err != nil || got != memory.ScopeUser {
		t.Errorf("scopeFor(\"\") = %q, %v", got, err)
	}
	if _, err := scopeFor("https://"); err == nil {
		t.Error("invalid project accepted")
	}
}

func TestDefaultTypeAndStatus(t *testing.T) {
	if defaultType("user") != memory.TypeSemantic || defaultType("project:x") != memory.TypeProject {
		t.Error("defaultType")
	}
	if statusFor(memory.TypePreference) != memory.StatusProposed {
		t.Error("preference should start proposed")
	}
	for _, typ := range []memory.Type{memory.TypeSemantic, memory.TypeProject, memory.TypeTemporary, memory.TypeEpisodic, memory.TypeCodebase} {
		if statusFor(typ) != memory.StatusActive {
			t.Errorf("%s should start active", typ)
		}
	}
}

func TestTTLFrom(t *testing.T) {
	if d, err := ttlFrom(0, time.Hour, 24*time.Hour); err != nil || d != time.Hour {
		t.Errorf("default = %v, %v", d, err)
	}
	if d, err := ttlFrom(60, 0, time.Hour); err != nil || d != time.Minute {
		t.Errorf("60s = %v, %v", d, err)
	}
	if d, err := ttlFrom(3600, 0, time.Hour); err != nil || d != time.Hour {
		t.Errorf("at max = %v, %v", d, err)
	}
	for _, bad := range []int{-1, 3601, 1 << 62} {
		if _, err := ttlFrom(bad, 0, time.Hour); err == nil {
			t.Errorf("ttlFrom(%d) accepted", bad)
		}
	}
}

func TestAgentOf(t *testing.T) {
	keyReq := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{Extra: map[string]any{apikey.ExtraAgent: "codex"}}}}
	if got := newHandlers(Deps{Agent: "claude-code"}).agentOf(keyReq); got != "codex" {
		t.Errorf("API key agent must win over configured agent, got %q", got)
	}
	if got := newHandlers(Deps{Agent: "claude-code"}).agentOf(&mcp.CallToolRequest{Extra: &mcp.RequestExtra{}}); got != "claude-code" {
		t.Errorf("configured agent = %q", got)
	}
	if got := newHandlers(Deps{}).agentOf(nil); got != memory.UnknownAgent {
		t.Errorf("fallback = %q", got)
	}
}

func TestToMemoryView(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	m := store.Memory{
		ID: "abc", Type: memory.TypeTemporary, Scope: "project:x", Content: "c",
		SourceAgent: "codex", Trust: memory.TrustAgent, ExpiresAt: &exp,
		Attrs: map[string]any{"kind": "handoff", "next_steps": []any{"a", 7, "b"}, "resumed_by": "opencode"},
	}
	v := toMemoryView(m)
	if v.ID != "abc" || v.Type != memory.TypeTemporary || v.Content != "c" || v.SourceAgent != "codex" || v.ExpiresAt != &exp {
		t.Errorf("view = %+v", v)
	}
	if len(v.NextSteps) != 2 || v.NextSteps[1] != "b" || v.ResumedBy != "opencode" {
		t.Errorf("handoff fields = %v / %q", v.NextSteps, v.ResumedBy)
	}
	m.Attrs = map[string]any{"next_steps": []any{"x"}} // not a handoff
	if v := toMemoryView(m); v.NextSteps != nil {
		t.Error("next_steps exposed for a non-handoff memory")
	}
}

func TestBudget(t *testing.T) {
	b := budget{left: 100, seen: map[string]bool{}}
	small := MemoryView{ID: "1", Content: strings.Repeat("a", 60)} // 60/3+40 = 60 tokens
	if _, ok := b.take(small); !ok || b.left != 40 {
		t.Fatalf("take small: left %d", b.left)
	}
	if _, ok := b.take(small); ok || b.truncated {
		t.Error("duplicate should be skipped without truncation")
	}
	if _, ok := b.take(MemoryView{ID: "2", Content: strings.Repeat("a", 60)}); ok || !b.truncated {
		t.Error("oversized memory accepted or truncation not flagged")
	}
	if _, ok := b.take(MemoryView{ID: "3"}); !ok { // 40 tokens fits exactly
		t.Error("memory that fits was rejected")
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("한국어테스트", 3); got != "한국어" {
		t.Errorf("got %q", got)
	}
	if got := truncateRunes("abc", 5); got != "abc" {
		t.Errorf("got %q", got)
	}
}
