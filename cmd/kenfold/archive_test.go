package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

func TestArchiveDryRunValidation(t *testing.T) {
	base := store.ArchivedMemory{
		ID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Type: memory.TypeSemantic,
		Scope: "project:github.com/acme/repo", Content: "Use PostgreSQL for storage.",
		SourceAgent: "codex", Trust: memory.TrustAgent, Confidence: 0.5, Status: memory.StatusActive,
	}
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	after := before.Add(time.Hour)
	for name, mutate := range map[string]func(*store.ArchivedMemory){
		"missing scope prefix": func(m *store.ArchivedMemory) { m.Scope = "acme" },
		"empty project":        func(m *store.ArchivedMemory) { m.Scope = "project:" },
		"local project":        func(m *store.ArchivedMemory) { m.Scope = "project:/tmp/repo" },
		"invalid trust":        func(m *store.ArchivedMemory) { m.Trust = "admin" },
		"invalid status":       func(m *store.ArchivedMemory) { m.Status = "unknown" },
		"invalid confidence":   func(m *store.ArchivedMemory) { m.Confidence = 1.1 },
		"invalid supersedes":   func(m *store.ArchivedMemory) { id := "nope"; m.Supersedes = &id },
		"self supersedes":      func(m *store.ArchivedMemory) { id := strings.ToUpper(m.ID); m.Supersedes = &id },
		"invalid valid range":  func(m *store.ArchivedMemory) { m.ValidFrom = &after; m.ValidTo = &before },
	} {
		t.Run(name, func(t *testing.T) {
			m := base
			mutate(&m)
			if err := dryRunArchive(t, m, ""); err == nil {
				t.Fatal("invalid archive passed dry-run")
			}
		})
	}
	for _, suffix := range []string{` {}`, ` trailing`, ` {"memory":{}}`} {
		if err := dryRunArchive(t, base, suffix); err == nil {
			t.Fatalf("accepted trailing data %q", suffix)
		}
	}
	for _, scope := range []string{"user", "project:github.com/acme/repo", "repo:github.com/acme/repo"} {
		m := base
		m.Scope = scope
		if err := dryRunArchive(t, m, ""); err != nil {
			t.Fatalf("valid scope %q: %v", scope, err)
		}
	}
}

func TestArchiveRejectsSecretsInMetadata(t *testing.T) {
	token := "ghp_" + strings.Repeat("Q7x", 12)
	for name, mutate := range map[string]func(*store.ArchivedMemory){
		"next steps": func(m *store.ArchivedMemory) { m.Attrs = map[string]any{"next_steps": []string{"use " + token}} },
		"nested arrays": func(m *store.ArchivedMemory) {
			m.Attrs = map[string]any{"nested": []any{nil, true, 3, []any{map[string]any{"evidence": token}}}}
		},
		"secret map key": func(m *store.ArchivedMemory) { m.Attrs = map[string]any{token: "value"} },
		"named credential": func(m *store.ArchivedMemory) {
			m.Attrs = map[string]any{"credentials": map[string]any{"api_key": "a9Q2v7R4z6P1t8M3"}}
		},
		"array credential": func(m *store.ArchivedMemory) {
			m.Attrs = map[string]any{"api_key": []any{"a9Q2v7R4z6P1t8M3"}}
		},
		"object credential": func(m *store.ArchivedMemory) {
			m.Attrs = map[string]any{"api_key": map[string]any{"value": "a9Q2v7R4z6P1t8M3"}}
		},
		"deep credential containers": func(m *store.ArchivedMemory) {
			m.Attrs = map[string]any{"api_key": []any{map[string]any{"wrapper": []any{"a9Q2v7R4z6P1t8M3"}}}}
		},
		"escaped private key": func(m *store.ArchivedMemory) {
			m.Attrs = map[string]any{"evidence": "-----BEGIN RSA PRIVATE KEY-----\nkey material\n-----END RSA PRIVATE KEY-----"}
		},
		"source session": func(m *store.ArchivedMemory) { m.SourceSession = &token },
		"evidence URI":   func(m *store.ArchivedMemory) { m.EvidenceURI = &token },
		"source agent":   func(m *store.ArchivedMemory) { m.SourceAgent = token },
	} {
		t.Run(name, func(t *testing.T) {
			m := store.ArchivedMemory{ID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Type: memory.TypeSemantic,
				Scope: "user", Content: "A harmless imported fact.", SourceAgent: "codex", Trust: memory.TrustAgent, Status: memory.StatusActive}
			mutate(&m)
			err := dryRunArchive(t, m, "")
			if err == nil {
				t.Fatal("archive metadata credential accepted")
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "a9Q2v7R4z6P1t8M3") {
				t.Fatal("archive validation echoed a credential")
			}
		})
	}
	m := store.ArchivedMemory{ID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Type: memory.TypeSemantic,
		Scope: "user", Content: "A harmless imported fact.", SourceAgent: "codex", Trust: memory.TrustAgent, Status: memory.StatusActive,
		Attrs: map[string]any{"next_steps": []string{"rotate the password"}, "api_key": []any{map[string]any{"value": "<your-key>"}}, "evidence": "Answer in Korean."}}
	if err := dryRunArchive(t, m, ""); err != nil {
		t.Errorf("benign metadata rejected: %v", err)
	}
	ref := store.ArchivedRef{MemoryID: m.ID, Scope: "project:github.com/acme/repo", Path: "src/a.go", Symbol: token, State: "pending"}
	if err := validRecord(store.ArchiveRecord{Ref: &ref}); err == nil {
		t.Error("code reference credential accepted")
	}
}

func dryRunArchive(t *testing.T, m store.ArchivedMemory, suffix string) error {
	t.Helper()
	header, _ := json.Marshal(store.ArchiveHeader{Format: store.ArchiveFormat})
	record, _ := json.Marshal(store.ArchiveRecord{Memory: &m})
	end, _ := json.Marshal(store.ArchiveRecord{End: &store.ArchiveCounts{Memories: 1}})
	input := string(header) + "\n" + string(record) + suffix + "\n" + string(end) + "\n"
	var output strings.Builder
	return run(context.Background(), []string{"import", "--dry-run", "-"}, func(key string) string {
		if key == "KENFOLD_DATABASE_URL" {
			return unreachableDB
		}
		return ""
	}, strings.NewReader(input), &output, &output)
}
