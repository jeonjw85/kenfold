package memory

import (
	"strings"
	"testing"
)

func TestParseType(t *testing.T) {
	for _, want := range Types {
		got, err := ParseType(string(want))
		if err != nil || got != want {
			t.Errorf("ParseType(%q) = %q, %v; want %q, nil", want, got, err, want)
		}
	}
	for _, bad := range []string{"", "Semantic", "working", " semantic"} {
		if _, err := ParseType(bad); err == nil {
			t.Errorf("ParseType(%q) succeeded; want error", bad)
		}
	}
}

func TestValidAgent(t *testing.T) {
	for _, ok := range []string{"claude-code", "codex", "opencode", "a", "gpt-5.1_local", "0agent"} {
		if !ValidAgent(ok) {
			t.Errorf("ValidAgent(%q) = false; want true", ok)
		}
	}
	for _, bad := range []string{"", "Claude", "my agent", "-codex", ".x", "a/b", strings.Repeat("a", 65)} {
		if ValidAgent(bad) {
			t.Errorf("ValidAgent(%q) = true; want false", bad)
		}
	}
}

func TestSanitizeAgent(t *testing.T) {
	for in, want := range map[string]string{
		"claude-code":           "claude-code",
		"Claude Code":           "claude-code",
		"codex-mcp-client":      "codex-mcp-client",
		"  OpenCode  ":          "opencode",
		"--weird--":             "weird--",
		"":                      UnknownAgent,
		"한국어":                   UnknownAgent, // every rune replaced by '-', then trimmed away
		strings.Repeat("x", 80): strings.Repeat("x", 64),
	} {
		if got := SanitizeAgent(in); got != want {
			t.Errorf("SanitizeAgent(%q) = %q; want %q", in, got, want)
		}
		if got := SanitizeAgent(in); !ValidAgent(got) {
			t.Errorf("SanitizeAgent(%q) = %q is not a valid agent", in, got)
		}
	}
}
