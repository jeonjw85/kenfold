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
