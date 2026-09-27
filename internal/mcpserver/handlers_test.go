package mcpserver

import (
	"context"
	"testing"

	"github.com/kenfold/kenfold/internal/memory"
)

// These cover the handler branches that return before touching the store, so
// they need no database. The store-backed paths are covered by the store
// integration tests and the E2E flow.

func TestRememberValidation(t *testing.T) {
	h := &handlers{} // store is never reached on these paths
	ctx := context.Background()

	t.Run("empty content is a tool error", func(t *testing.T) {
		_, _, err := h.remember(ctx, nil, RememberInput{Content: "   "})
		if err == nil {
			t.Fatal("want tool error for empty content")
		}
	})

	t.Run("temporary without ttl is a tool error", func(t *testing.T) {
		_, _, err := h.remember(ctx, nil, RememberInput{
			Content: "wip", Type: memory.TypeTemporary,
		})
		if err == nil {
			t.Fatal("want tool error for temporary without ttl")
		}
	})
}

func TestForgetValidation(t *testing.T) {
	h := &handlers{}
	if _, _, err := h.forget(context.Background(), nil, ForgetInput{ID: " "}); err == nil {
		t.Fatal("want tool error for empty id")
	}
}

func TestScopeFor(t *testing.T) {
	cases := map[string]string{
		"":                           "user",
		"  ":                         "user",
		"github.com/kenfold/kenfold": "project:github.com/kenfold/kenfold",
		"https://github.com/o/r.git": "project:github.com/o/r",
		"git@github.com:o/r.git":     "project:github.com/o/r",
		"ssh://git@example.com/o/r/": "project:example.com/o/r",
		"my-local-project":           "project:my-local-project",
	}
	for in, want := range cases {
		if got := scopeFor(in); got != want {
			t.Errorf("scopeFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusFor(t *testing.T) {
	if statusFor(memory.TypePreference) != memory.StatusProposed {
		t.Error("preference should start proposed")
	}
	for _, typ := range []memory.Type{memory.TypeSemantic, memory.TypeProject, memory.TypeTemporary} {
		if statusFor(typ) != memory.StatusActive {
			t.Errorf("%s should start active", typ)
		}
	}
}

func TestAgentNameFallback(t *testing.T) {
	if got := agentName(nil); got != "unknown" {
		t.Errorf("agentName(nil) = %q, want unknown", got)
	}
}
