package apikey

import (
	"bytes"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

func TestGenerate(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		k, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if len(k) != KeyLen || !strings.HasPrefix(k, Prefix) || !WellFormed(k) {
			t.Fatalf("bad key %q (len %d)", k, len(k))
		}
		if seen[k] {
			t.Fatal("duplicate key generated")
		}
		seen[k] = true
	}
}

func TestWellFormed(t *testing.T) {
	good, _ := Generate()
	for _, bad := range []string{
		"", "kf_", "kf_short",
		"xx_" + good[3:],                   // wrong prefix
		good + "A",                         // too long
		good[:KeyLen-1] + "=",              // padding/invalid char
		good[:KeyLen-1] + " ",              // whitespace
		strings.Replace(good, "_", "-", 1), // "kf-..."
	} {
		if WellFormed(bad) {
			t.Errorf("WellFormed(%q) = true", bad)
		}
	}
}

func TestHash(t *testing.T) {
	a, _ := Generate()
	b, _ := Generate()
	if len(Hash(a)) != 32 {
		t.Fatalf("hash length %d", len(Hash(a)))
	}
	if !bytes.Equal(Hash(a), Hash(a)) || bytes.Equal(Hash(a), Hash(b)) {
		t.Error("hash is not a deterministic, distinct digest")
	}
}

func TestAgentFrom(t *testing.T) {
	if _, ok := AgentFrom(nil); ok {
		t.Error("nil TokenInfo yielded an agent")
	}
	if _, ok := AgentFrom(&auth.TokenInfo{Extra: map[string]any{ExtraAgent: ""}}); ok {
		t.Error("empty agent accepted")
	}
	if _, ok := AgentFrom(&auth.TokenInfo{Extra: map[string]any{ExtraAgent: 42}}); ok {
		t.Error("non-string agent accepted")
	}
	if a, ok := AgentFrom(&auth.TokenInfo{Extra: map[string]any{ExtraAgent: "codex"}}); !ok || a != "codex" {
		t.Errorf("AgentFrom = %q, %v", a, ok)
	}
}
