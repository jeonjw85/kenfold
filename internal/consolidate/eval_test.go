package consolidate

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

// TestEvalModel measures pair judgements and a digest against a real model
// (make eval). Results are recorded in testdata/RESULTS.md.
func TestEvalModel(t *testing.T) {
	url := os.Getenv("KENFOLD_EVAL_CHAT_URL")
	if url == "" {
		t.Skip("KENFOLD_EVAL_CHAT_URL not set")
	}
	model := os.Getenv("KENFOLD_EVAL_CHAT_MODEL")
	if model == "" {
		model = "qwen3.5:4b"
	}
	c, err := chat.New(chat.Config{BaseURL: url, Model: model, Reasoning: os.Getenv("KENFOLD_EVAL_CHAT_REASONING"), Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/eval.json")
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		Pairs []struct {
			ID, A, B, Want, Keep string
		}
		Digest []string
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	day := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	mk := func(id, content string, at time.Time) store.Memory {
		return store.Memory{ID: id, Type: memory.TypeProject, Scope: "project:github.com/acme/app", Content: content, SourceAgent: "codex", CreatedAt: at}
	}
	relation := map[string]string{store.KindDuplicate: "same", store.KindConflict: "conflict", store.KindDistinct: "distinct"}
	var correct, wrongKeep, falseRetire, missed int
	var total time.Duration
	for _, p := range set.Pairs {
		a, b := mk("A", p.A, day), mk("B", p.B, day.Add(72*time.Hour))
		start := time.Now()
		v, err := Judge(ctx, c, a, b)
		total += time.Since(start)
		if err != nil {
			t.Errorf("%s: %v", p.ID, err)
			continue
		}
		got := relation[v.Kind]
		switch {
		case got == p.Want:
			correct++
			if p.Keep != "" && v.Keep != p.Keep {
				wrongKeep++
			}
		case p.Want == "distinct":
			falseRetire++ // would retire a memory that is still needed
		default:
			missed++
		}
		t.Logf("%-20s want %-8s got %-8s keep %s  %.1fs  %s", p.ID, p.Want, got, v.Keep, time.Since(start).Seconds(), v.Reason)
	}
	t.Logf("pairs: %d/%d correct, %d wrong keep, %d distinct pairs judged same or conflict, %d missed; %.1fs per pair",
		correct, len(set.Pairs), wrongKeep, falseRetire, missed, total.Seconds()/float64(len(set.Pairs)))

	var sessions []store.Memory
	for i, s := range set.Digest {
		sessions = append(sessions, mk("s"+strconv.Itoa(i), s, day.Add(time.Duration(i)*24*time.Hour)))
	}
	start := time.Now()
	d, err := Digest(ctx, c, "project:github.com/acme/app", sessions)
	if err != nil {
		t.Errorf("digest: %v", err)
		return
	}
	t.Logf("digest (%.1fs):\n%s", time.Since(start).Seconds(), d)
}
