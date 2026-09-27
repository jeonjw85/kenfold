package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
)

type evalSet struct {
	Extraction []struct {
		ID      string `json:"id"`
		Split   string `json:"split"` // "" (dev: used to tune the prompt) or "holdout"
		Project string `json:"project"`
		Session string `json:"session"`
		Expect  []struct {
			Name  string     `json:"name"`
			Types []string   `json:"types"`
			Any   [][]string `json:"any"`
		} `json:"expect"`
		Forbid []string `json:"forbid"`
	} `json:"extraction"`
	Classification []struct {
		Content string   `json:"content"`
		Project bool     `json:"project"`
		Want    []string `json:"want"`
	} `json:"classification"`
}

// TestEvalModel measures extraction and classification against a real chat
// model. It is opt-in:
//
//	KENFOLD_EVAL_CHAT_URL=http://127.0.0.1:11435/v1 KENFOLD_EVAL_CHAT_MODEL=kenfold-extract \
//	  go test -run TestEvalModel -v ./internal/extract/
//
// It reports per-case results and aggregate metrics, and fails if forbidden
// content (secrets, injected instructions, one-off tasks) is extracted, or if
// recall or classification accuracy fall below the floors below.
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
	var set evalSet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	// A realistic secret, assembled at run time.
	secret := "ghp_" + strings.Repeat("Tq5", 12)
	ctx := context.Background()

	var expected, found, extracted, forbiddenHits, typeOK, typeTotal int
	split := map[string]*[2]int{"dev": {}, "holdout": {}} // found, expected
	var elapsed time.Duration
	var report strings.Builder
	for _, cse := range set.Extraction {
		sp := cse.Split
		if sp == "" {
			sp = "dev"
		}
		session := strings.ReplaceAll(cse.Session, "{{SECRET}}", secret)
		start := time.Now()
		res, err := Extract(ctx, c, Source{Project: cse.Project, Content: session}, Options{})
		dt := time.Since(start)
		elapsed += dt
		if err != nil {
			t.Errorf("%s: %v (raw %q)", cse.ID, err, res.Raw)
			continue
		}
		extracted += len(res.Candidates)
		fmt.Fprintf(&report, "\n== %s [%s] (%.1fs, %d kept, %d rejected, %d+%d tokens)\n", cse.ID, sp, dt.Seconds(), len(res.Candidates), len(res.Rejected), res.Usage.PromptTokens, res.Usage.CompletionTokens)
		for _, m := range res.Candidates {
			scope := "project"
			if m.UserScope {
				scope = "user"
			}
			fmt.Fprintf(&report, "   + [%s/%s %.2f] %s\n", m.Type, scope, m.Confidence, m.Content)
		}
		for _, r := range res.Rejected {
			fmt.Fprintf(&report, "   - rejected (%s): %s\n", r.Reason, r.Content)
		}
		for _, e := range cse.Expect {
			expected++
			hit := false
			for _, m := range res.Candidates {
				if !slices.Contains(e.Types, string(m.Type)) {
					continue
				}
				low := strings.ToLower(m.Content)
				for _, group := range e.Any {
					all := true
					for _, w := range group {
						if !strings.Contains(low, strings.ToLower(w)) {
							all = false
							break
						}
					}
					if all {
						hit = true
					}
				}
			}
			if hit {
				found++
				split[sp][0]++
			}
			split[sp][1]++
			fmt.Fprintf(&report, "   expect %-32s %v\n", e.Name, map[bool]string{true: "FOUND", false: "missing"}[hit])
		}
		// Secrets must appear nowhere (evidence is stored too). Topic words
		// (one-off tasks, injected instructions) are checked against the memory
		// itself: the evidence is a quote of the session and may mention them.
		for _, m := range res.Candidates {
			all := strings.ToLower(m.Content + " " + m.Evidence)
			content := strings.ToLower(m.Content)
			if strings.Contains(all, strings.ToLower(secret)) {
				forbiddenHits++
				fmt.Fprintf(&report, "   FORBIDDEN secret in: %s\n", m.Content)
			}
			for _, f := range cse.Forbid {
				f = strings.ToLower(strings.ReplaceAll(f, "{{SECRET}}", secret))
				if strings.Contains(content, f) {
					forbiddenHits++
					fmt.Fprintf(&report, "   FORBIDDEN %q in: %s\n", f, m.Content)
				}
			}
		}
	}

	fmt.Fprintf(&report, "\n== classification\n")
	for _, cse := range set.Classification {
		typ, conf, err := Classify(ctx, c, cse.Content, cse.Project)
		typeTotal++
		ok := err == nil && slices.Contains(cse.Want, string(typ))
		if ok {
			typeOK++
		}
		fmt.Fprintf(&report, "   %-5v %-10s (%.2f) want %v: %s\n", ok, typ, conf, cse.Want, cse.Content)
		if err != nil {
			fmt.Fprintf(&report, "         error: %v\n", err)
		}
	}

	recall := float64(found) / float64(max(expected, 1))
	accuracy := float64(typeOK) / float64(max(typeTotal, 1))
	fmt.Fprintf(&report, "\n== summary (model %s)\n   extraction recall %d/%d = %.0f%% (dev %d/%d, holdout %d/%d), %d memories extracted, forbidden hits %d, total extraction time %.0fs\n   classification accuracy %d/%d = %.0f%%\n",
		model, found, expected, 100*recall, split["dev"][0], split["dev"][1], split["holdout"][0], split["holdout"][1], extracted, forbiddenHits, elapsed.Seconds(), typeOK, typeTotal, 100*accuracy)
	t.Log(report.String())

	if forbiddenHits > 0 {
		t.Errorf("%d forbidden extractions (secrets, injected instructions, or one-off tasks)", forbiddenHits)
	}
	if recall < 0.7 {
		t.Errorf("extraction recall %.0f%% is below 70%%", 100*recall)
	}
	if accuracy < 0.7 {
		t.Errorf("classification accuracy %.0f%% is below 70%%", 100*accuracy)
	}
}
