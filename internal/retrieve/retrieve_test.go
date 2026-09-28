package retrieve

import (
	"math"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

func TestTemporalIntent(t *testing.T) {
	for q, want := range map[string]bool{
		"what did we do in the last session about webhooks?": true,
		"what was the most recent thing we worked on?":       true,
		"latest decision on caching":                         true,
		"what happened yesterday":                            true,
		"지난번 배포 실패 원인이 뭐였지?":                                 true,
		"최근에 로그인 쪽 뭐 고쳤어?":                                   true,
		"어제 작업 이어서":                                          true,
		"we use last-write-wins for conflicts":               false,
		"how do we store money":                              false,
		"which migration tool do we use":                     false,
		"ORM 써도 돼?":                                          false,
	} {
		if got := temporalIntent(q); got != want {
			t.Errorf("temporalIntent(%q) = %v", q, got)
		}
	}
}

func TestRecencyFactor(t *testing.T) {
	now := time.Now()
	at := func(typ memory.Type, days float64) store.Memory {
		return store.Memory{Type: typ, CreatedAt: now.Add(-time.Duration(days * float64(24*time.Hour)))}
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if f := recencyFactor(at(memory.TypeProject, 400), now, false, true); f != 1 {
		t.Errorf("facts do not age without temporal intent: %v", f)
	}
	if f := recencyFactor(at(memory.TypeEpisodic, 0), now, false, true); !near(f, 1) {
		t.Errorf("new summary = %v", f)
	}
	if f := recencyFactor(at(memory.TypeEpisodic, 30), now, false, true); !near(f, 1-episodicAgeLoss/2) {
		t.Errorf("summary at one half-life = %v", f)
	}
	if f := recencyFactor(at(memory.TypeEpisodic, 3000), now, false, true); f < 1-episodicAgeLoss-1e-9 {
		t.Errorf("old summaries keep at least %v: %v", 1-episodicAgeLoss, f)
	}
	if f := recencyFactor(at(memory.TypeEpisodic, 30), now, false, false); f != 1 {
		t.Errorf("summaries do not age on uncalibrated scores: %v", f)
	}
	if f := recencyFactor(at(memory.TypeProject, 7), now, true, false); !near(f, 0.5) {
		t.Errorf("with intent, one half-life = %v", f)
	}
	if f := recencyFactor(store.Memory{CreatedAt: now.Add(time.Hour)}, now, true, true); f != 1 {
		t.Errorf("future timestamps count as new: %v", f)
	}
}

func TestStaleFactor(t *testing.T) {
	ref := func(sym, state string) store.CodeRef { return store.CodeRef{Symbol: sym, State: state} }
	for _, c := range []struct {
		refs []store.CodeRef
		want float64
	}{
		{nil, 1},
		{[]store.CodeRef{ref("", store.RefCurrent), ref("", store.RefChanged)}, 1}, // a changed file alone is normal
		{[]store.CodeRef{ref("Dedupe", store.RefChanged)}, staleChanged},
		{[]store.CodeRef{ref("Dedupe", store.RefChanged), ref("", store.RefMissing)}, staleMissing},
		{[]store.CodeRef{ref("X", store.RefPending), ref("Y", store.RefUnresolved)}, 1},
	} {
		if got := staleFactor(c.refs); got != c.want {
			t.Errorf("staleFactor(%+v) = %v, want %v", c.refs, got, c.want)
		}
	}
}

func TestSortCandidates(t *testing.T) {
	now := time.Now()
	mk := func(id string, score float64, reranked bool, age time.Duration) *candidate {
		return &candidate{m: store.Memory{ID: id, CreatedAt: now.Add(-age)}, score: score, reranked: reranked}
	}
	cs := []*candidate{mk("low", 0.1, true, 0), mk("rest", 0.9, false, 0), mk("high", 0.8, true, time.Hour), mk("tie-new", 0.8, true, 0)}
	sortCandidates(cs)
	var got []string
	for _, c := range cs {
		got = append(got, c.m.ID)
	}
	want := []string{"tie-new", "high", "low", "rest"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestClampScore(t *testing.T) {
	for in, want := range map[float64]float64{0.5: 0.5, 2: 1, 0: 1e-6, -1: 1e-6, math.NaN(): 1e-6} {
		if got := clampScore(in); got != want {
			t.Errorf("clampScore(%v) = %v", in, got)
		}
	}
}
