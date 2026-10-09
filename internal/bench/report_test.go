package bench

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The live run exposed this break: a later local ingestion error discarded
// already completed dataset results before the harness wrote its report.
func TestBenchNonBudgetFailurePreservesCompletedResults(t *testing.T) {
	if os.Getenv("KENFOLD_BENCH_REPORT_FIXTURE") == "1" {
		rep := Report{Sets: []SetResult{
			{Name: "fixture raw", Rows: []Row{{Label: "all", N: 1, F1: 1, HasF1: true, Judge: 1}}},
			{Name: "fixture extraction", Rows: []Row{{Label: "all", N: 1, F1: 0, HasF1: true, Judge: 0}}},
		}}
		reportBench(t, rep, errors.New("fixture local embedding timeout"), nil, 0)
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestBenchNonBudgetFailurePreservesCompletedResults$", "-test.v")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "KENFOLD_BENCH_REPORT_FIXTURE=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "fixture local embedding timeout") {
		t.Fatalf("unfinished evaluation must still fail with the original error: %v\n%s", err, out)
	}
	md, err := os.ReadFile(filepath.Join(dir, "testdata", "RESULTS.md"))
	if err != nil {
		t.Fatalf("completed results lost on a non-budget failure: %v\n%s", err, out)
	}
	for _, want := range []string{
		"PARTIAL", "## fixture raw", "## fixture extraction",
		"| all | 1 | 1.000 | 1.000 |", "| all | 1 | 0.000 | 0.000 |",
	} {
		if !strings.Contains(string(md), want) {
			t.Errorf("saved report missing %q:\n%s", want, md)
		}
	}
}
