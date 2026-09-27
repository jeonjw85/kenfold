package store

import (
	"math"
	"strings"
	"testing"
)

func TestValidID(t *testing.T) {
	for _, ok := range []string{"0199a0e1-fae4-703c-9169-61ede9d496fd", "11111111-1111-1111-1111-111111111111", "ABCDEF01-2345-6789-abcd-ef0123456789"} {
		if !ValidID(ok) {
			t.Errorf("ValidID(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "x", "0199a0e1fae4703c916961ede9d496fd", "0199a0e1-fae4-703c-9169-61ede9d496f", "0199a0e1-fae4-703c-9169-61ede9d496fg", "0199a0e1_fae4-703c-9169-61ede9d496fd", "'; DROP TABLE memory; --xxxxxxxxxxxx"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
}

func TestVectorParam(t *testing.T) {
	if v, err := vectorParam(nil); v != nil || err != nil {
		t.Errorf("empty = %v, %v; want nil, nil", v, err)
	}
	if _, err := vectorParam(make([]float32, 3)); err == nil {
		t.Error("wrong dimension accepted")
	}
	vec := make([]float32, EmbeddingDim)
	vec[0], vec[1] = 0.5, -1.25e-7
	lit, err := vectorParam(vec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(*lit, "[0.5,-1.25e-07,0,") || !strings.HasSuffix(*lit, ",0]") || strings.Count(*lit, ",") != EmbeddingDim-1 {
		t.Errorf("literal = %.40s...", *lit)
	}
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		vec[2] = bad
		if _, err := vectorParam(vec); err == nil {
			t.Errorf("value %v accepted", bad)
		}
	}
}

func TestClamp(t *testing.T) {
	for _, c := range []struct{ v, def, max, want int }{{0, 10, 50, 10}, {-5, 10, 50, 10}, {7, 10, 50, 7}, {500, 10, 50, 50}} {
		if got := clamp(c.v, c.def, c.max); got != c.want {
			t.Errorf("clamp(%d,%d,%d) = %d, want %d", c.v, c.def, c.max, got, c.want)
		}
	}
}

func TestSearchSQLColumns(t *testing.T) {
	// The search result is scanned with fields() + score; keep them in lockstep.
	var m Memory
	if got, want := len(fields(&m)), len(columnList); got != want {
		t.Fatalf("fields() has %d entries, columnList %d", got, want)
	}
	if !strings.Contains(searchSQL, mColumns) || !strings.Contains(searchSQL, "1.0 / (60 + rnk)") || !strings.Contains(searchSQL, "* 61 /") {
		t.Error("searchSQL not built as expected")
	}
}
