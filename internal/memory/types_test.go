package memory

import "testing"

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
