package coderef

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := &testRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.email", "test@example.com")
	r.git("config", "user.name", "test")
	r.git("config", "commit.gpgsign", "false")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *testRepo) write(path, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) commit(msg string) string {
	r.git("add", "-A")
	r.git("commit", "-q", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

func (r *testRepo) check(targets ...Target) map[Target]Result {
	r.t.Helper()
	repo, err := Open(context.Background(), r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	res, err := repo.Check(context.Background(), targets)
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[Target]Result{}
	for _, x := range res {
		for _, t := range targets {
			if t.Path == x.Path && t.Symbol == x.Symbol && t.AnchorCommit == x.AnchorCommit {
				out[t] = x
			}
		}
	}
	return out
}

const handlerV1 = `package webhook

func dedupePayload(b []byte) string { return "hash" }

func Handle() { dedupePayload(nil) }
`

func TestRepoCheck(t *testing.T) {
	r := newTestRepo(t)
	if _, err := Open(context.Background(), r.dir); err != ErrNoCommits {
		t.Fatalf("Open on empty repo: %v", err)
	}
	r.write("internal/webhook/handler.go", handlerV1)
	r.write("internal/jobs/workers.go", "package jobs\n\nfunc Register() {}\n")
	c1 := r.commit("one")

	file := Target{Path: "internal/webhook/handler.go"}
	sym := Target{Path: "internal/webhook/handler.go", Symbol: "dedupePayload"}
	loose := Target{Symbol: "Register"}
	absent := Target{Symbol: "NoSuchThing"}
	bad := Target{Path: "../etc/passwd"}
	got := r.check(file, sym, loose, absent, bad)
	if x := got[file]; !x.Found || !strings.HasPrefix(x.Hash, hashPrefixBlob) {
		t.Errorf("file = %+v", x)
	}
	if x := got[sym]; !x.Found || !strings.HasPrefix(x.Hash, hashPrefixSym) {
		t.Errorf("symbol = %+v", x)
	}
	if x := got[loose]; !x.Found || x.ResolvedPath != "internal/jobs/workers.go" {
		t.Errorf("path-less symbol = %+v", x)
	}
	if x, ok := got[absent]; !ok || x.Found {
		t.Errorf("absent symbol = %+v, %v", x, ok)
	}
	if _, ok := got[bad]; ok {
		t.Error("invalid target was checked")
	}
	symHash, fileHash := got[sym].Hash, got[file].Hash

	// Reformatting and edits elsewhere in the file keep the symbol's hash; the file's changes.
	r.write("internal/webhook/handler.go", strings.Replace(handlerV1, "func Handle() { dedupePayload(nil) }", "func Handle() {\n\tdedupePayload(nil)\n\tlog()\n}", 1)+"\nfunc log() {}\n")
	c2 := r.commit("two")
	anchoredSym := Target{Path: sym.Path, Symbol: sym.Symbol, AnchorCommit: c1, AnchorHash: symHash}
	anchoredFile := Target{Path: file.Path, AnchorCommit: c1, AnchorHash: fileHash}
	got = r.check(anchoredSym, anchoredFile)
	if x := got[anchoredSym]; !x.Found || x.Hash != symHash {
		t.Errorf("symbol after unrelated edit = %+v, want hash %s", x, symHash)
	}
	if x := got[anchoredFile]; !x.Found || x.Hash == fileHash {
		t.Errorf("file after edit = %+v", x)
	}

	// Removing the symbol: found=false.
	r.write("internal/webhook/handler.go", "package webhook\n\nfunc Handle() {}\n")
	r.commit("three")
	if x := r.check(anchoredSym)[anchoredSym]; x.Found {
		t.Errorf("removed symbol = %+v", x)
	}

	// An anchor HEAD does not contain (another branch) is skipped.
	r.git("checkout", "-q", "-b", "old", c1)
	r.write("other.go", "package x\n")
	r.commit("side")
	r.git("checkout", "-q", "main")
	side := r.git("rev-parse", "old")
	onSide := Target{Path: file.Path, AnchorCommit: side, AnchorHash: "x"}
	unknown := Target{Path: file.Path, AnchorCommit: strings.Repeat("ab", 20), AnchorHash: "x"}
	if got := r.check(onSide, unknown); len(got) != 0 {
		t.Errorf("non-ancestor anchors were checked: %+v", got)
	}
	_ = c2

	// Pending references to files with uncommitted changes wait; anchored ones are still checked.
	r.write("internal/jobs/workers.go", "package jobs\n\nfunc Register() { changed() }\n")
	pendingDirty := Target{Path: "internal/jobs/workers.go", Symbol: "Register"}
	anchoredDirty := Target{Path: "internal/jobs/workers.go", AnchorCommit: c1, AnchorHash: "x"}
	got = r.check(pendingDirty, anchoredDirty, loose)
	if _, ok := got[pendingDirty]; ok {
		t.Error("pending reference to a dirty file was anchored")
	}
	if _, ok := got[loose]; ok {
		t.Error("pending path-less reference resolved to a dirty file was anchored")
	}
	if x, ok := got[anchoredDirty]; !ok || !x.Found {
		t.Errorf("anchored reference to a dirty file = %+v, %v", x, ok)
	}
}

func TestValidTargetRules(t *testing.T) {
	for _, tc := range []struct {
		t  Target
		ok bool
	}{
		{Target{Path: "a/b.go"}, true},
		{Target{Path: "Makefile"}, true},
		{Target{Symbol: "Server.Dedupe"}, true},
		{Target{Path: "a/b.go", Symbol: "x", AnchorCommit: "abc1234"}, true},
		{Target{}, false},
		{Target{Path: "/etc/passwd"}, false},
		{Target{Path: "a/../b.go"}, false},
		{Target{Path: "-rf.go"}, true}, // passed after "--", never as an option
		{Target{Symbol: "a b"}, false},
		{Target{Symbol: "--output=x"}, false},
		{Target{Path: "a.go", AnchorCommit: "HEAD"}, false},
		{Target{Path: "a.go", AnchorCommit: "--all"}, false},
	} {
		if got := ValidTarget(tc.t); got != tc.ok {
			t.Errorf("ValidTarget(%+v) = %v", tc.t, got)
		}
	}
}

func TestRepoCheckUsesOpenedCommit(t *testing.T) {
	r := newTestRepo(t)
	r.write("jobs/workers.go", "package jobs\n\nfunc Register() {}\n")
	opened := r.commit("before")
	repo, err := Open(context.Background(), r.dir)
	if err != nil {
		t.Fatal(err)
	}
	targets := []Target{{Path: "jobs/workers.go"}, {Path: "jobs/workers.go", Symbol: "Register"}, {Symbol: "Register"}}
	before, err := repo.Check(context.Background(), targets)
	if err != nil || len(before) != len(targets) {
		t.Fatalf("initial check: %+v, %v", before, err)
	}
	r.write("jobs/workers.go", "package jobs\n\nfunc Replacement() {}\n")
	later := r.commit("after")
	// Even if HEAD moves after Open, every result must match the commit
	// Sync will report, including symbol search and ancestry decisions.
	after, err := repo.Check(context.Background(), append(targets, Target{Path: "jobs/workers.go", AnchorCommit: later}))
	if err != nil || len(after) != len(before) {
		t.Fatalf("check after HEAD moved: %+v, %v", after, err)
	}
	if repo.Head() != opened {
		t.Fatalf("opened commit = %q, want %q", repo.Head(), opened)
	}
	for i := range before {
		if after[i] != before[i] {
			t.Errorf("target %v changed when HEAD moved: got %+v, want %+v", targets[i], after[i], before[i])
		}
	}
}

func TestRepoCheckIncomplete(t *testing.T) {
	r := newTestRepo(t)
	r.write("jobs/workers.go", "package jobs\n\nfunc Register() {}\n")
	r.write("jobs/broken.go", "package jobs\n\nfunc (( Register oops {\n") // mentions Register, does not parse
	r.write("docs/notes.md", "Register is called at startup.\n")
	c1 := r.commit("one")

	pendingLoose := Target{Symbol: "Register"}
	anchoredLoose := Target{Symbol: "Register", AnchorCommit: c1, AnchorHash: "sym:x"}
	brokenFile := Target{Path: "jobs/broken.go", Symbol: "Missing", AnchorCommit: c1, AnchorHash: "sym:y"}
	got := r.check(pendingLoose, anchoredLoose, brokenFile)
	// A definition was found, but another code file mentioning the name could not be read: no verdict.
	if x, ok := got[pendingLoose]; ok {
		t.Errorf("partial lookup anchored: %+v", x)
	}
	if x, ok := got[anchoredLoose]; ok {
		t.Errorf("partial lookup checked: %+v", x)
	}
	// A symbol not found in a file that did not parse is not reported missing.
	if x, ok := got[brokenFile]; ok {
		t.Errorf("unparsable file reported: %+v", x)
	}

	// Once the file parses, the verdict is definitive (and a mention in docs does not matter).
	r.write("jobs/broken.go", "package jobs\n\nfunc helper() { Register() }\n")
	r.commit("fix")
	got = r.check(pendingLoose, anchoredLoose)
	if x := got[pendingLoose]; !x.Found || x.ResolvedPath != "jobs/workers.go" {
		t.Errorf("after fix, pending = %+v", x)
	}
	if x, ok := got[anchoredLoose]; !ok || !x.Found {
		t.Errorf("after fix, anchored = %+v, %v", x, ok)
	}
}

func TestDefinitionsIncomplete(t *testing.T) {
	if _, err := definitions("x.go", []byte("package x\n\nfunc (( oops {\n"), "Missing"); err != errIncomplete {
		t.Errorf("broken file without the symbol: %v", err)
	}
	defs, err := definitions("x.go", []byte("package x\n\nfunc Found() {}\n\nfunc (( oops {\n"), "Found")
	if err != nil || len(defs) != 1 {
		t.Errorf("broken file with the symbol: %q, %v", defs, err)
	}
}
