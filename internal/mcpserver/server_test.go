package mcpserver

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := New("test").Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestInstructions(t *testing.T) {
	if n := len(Instructions); n > 512 {
		t.Errorf("Instructions is %d chars; keep <= 512 (Codex only guarantees the first 512)", n)
	}
	cs := connect(t)
	if got := cs.InitializeResult().Instructions; got != Instructions {
		t.Errorf("initialize instructions = %q", got)
	}
}

func TestToolsList(t *testing.T) {
	cs := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		byName[tool.Name] = tool
		if tool.Description == "" || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("tool %s is missing description or schemas", tool.Name)
		}
	}
	slices.Sort(names)
	want := []string{"forget", "get_context", "handoff", "recall", "remember", "resume"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	for _, n := range []string{"get_context", "recall", "resume"} {
		if a := byName[n].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s should be read-only", n)
		}
	}
	if a := byName["forget"].Annotations; a == nil || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Error("forget should be marked destructive")
	}

	// Domain enums must be visible to agents in the input schema.
	schema := byName["remember"].InputSchema.(map[string]any)
	typeProp := schema["properties"].(map[string]any)["type"].(map[string]any)
	enum, _ := typeProp["enum"].([]any)
	if len(enum) != 6 {
		t.Errorf("remember.type enum = %v, want 6 memory types", typeProp["enum"])
	}
	if req, _ := schema["required"].([]any); len(req) != 1 || req[0] != "content" {
		t.Errorf("remember required = %v, want [content]", schema["required"])
	}
}

func TestStubReturnsToolError(t *testing.T) {
	cs := connect(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "remember",
		Arguments: map[string]any{"content": "we use pnpm", "type": "project"},
	})
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError result from stub")
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "not implemented") {
		t.Errorf("error text = %q", text)
	}
}

func TestInvalidInputRejected(t *testing.T) {
	cs := connect(t)
	for name, args := range map[string]map[string]any{
		"unknown type":    {"content": "x", "type": "working"},
		"missing content": {"type": "project"},
		"unknown field":   {"content": "x", "colour": "red"},
	} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "remember", Arguments: args})
		if err == nil && !res.IsError {
			t.Errorf("%s: call succeeded; want validation failure", name)
			continue
		}
		// Validation must fail before reaching the stub handler.
		if err == nil && strings.Contains(res.Content[0].(*mcp.TextContent).Text, "not implemented") {
			t.Errorf("%s: reached handler; want schema validation failure", name)
		}
	}
}
