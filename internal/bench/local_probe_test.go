package bench

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/embed"
	"github.com/kenfold/kenfold/internal/store"
)

// This opt-in reproduction only calls the dedicated loopback embedding model.
// It never calls Run, a reader/judge, a database, or the paid task runner.
func TestLocalLongMemEvalEmbeddingRecovery(t *testing.T) {
	if os.Getenv("KENFOLD_LOCAL_EMBED_PROBE") != "1" {
		t.Skip("local embedding recovery probe is opt-in")
	}
	dir, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	qs, err := LoadLongMemEval(dir, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 60 {
		t.Fatalf("expected the unchanged 60-question selection, got %d", len(qs))
	}
	emb, err := embed.New(embed.Config{
		BaseURL: "http://127.0.0.1:11437/v1", Model: "kenfold-embed", Name: "bge-m3",
		Dim: store.EmbeddingDim, BatchSize: embed.DefaultBatchSize, Timeout: 3 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	questions, calls, inputs := 0, 0, 0
	for _, q := range qs {
		if os.Getenv("KENFOLD_LOCAL_EMBED_PROBE_ALL") != "1" && q.ID != "15745da0_abs" {
			continue
		}
		texts := make([]string, len(q.Turns))
		for i, turn := range q.Turns {
			texts[i] = clipBytes(turn.Content, maxContentBytes)
		}
		for start := 0; start < len(texts); start += embed.DefaultBatchSize {
			batch := texts[start:min(start+embed.DefaultBatchSize, len(texts))]
			encoded, err := json.Marshal(batch)
			if err != nil {
				t.Fatal(err)
			}
			began := time.Now()
			vecs, err := emb.Embed(ctx, batch)
			if err != nil {
				t.Fatalf("local input %s batch %d/%d after %s: %v", q.ID, start/embed.DefaultBatchSize+1, (len(texts)+31)/32, time.Since(began), err)
			}
			if len(vecs) != len(batch) {
				t.Fatalf("input %s: vector count %d, want %d", q.ID, len(vecs), len(batch))
			}
			for _, vec := range vecs {
				if len(vec) != store.EmbeddingDim {
					t.Fatalf("input %s: incorrect vector dimensions", q.ID)
				}
				for _, v := range vec {
					if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
						t.Fatalf("input %s: nonfinite vector", q.ID)
					}
				}
			}
			calls++
			inputs += len(batch)
			t.Logf("local input %s batch %d: %d vectors, duration=%s, source_sha256=%x", q.ID, start/embed.DefaultBatchSize+1, len(batch), time.Since(began), sha256.Sum256(encoded))
		}
		questions++
	}
	if questions == 0 {
		t.Fatal("formerly failing input was not selected")
	}
	t.Logf("local embedding recovery: %d question haystacks, %d HTTP batches, %d finite vectors; zero reader/judge or database calls", questions, calls, inputs)
}
