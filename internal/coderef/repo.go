package coderef

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Target is a reference to check, as the server lists it. AnchorCommit and
// AnchorHash are empty for references that have not been anchored yet.
type Target struct {
	Path         string `json:"path"`
	Symbol       string `json:"symbol"`
	AnchorCommit string `json:"anchor_commit,omitempty"`
	AnchorHash   string `json:"anchor_hash,omitempty"`
}

// Result is the state of a Target's code at the checked commit. Found is
// false when the file or symbol does not exist there.
type Result struct {
	Path         string `json:"path"`
	Symbol       string `json:"symbol"`
	AnchorCommit string `json:"anchor_commit,omitempty"`
	Found        bool   `json:"found"`
	Hash         string `json:"hash,omitempty"`
	ResolvedPath string `json:"resolved_path,omitempty"`
}

const (
	maxTargets     = 500
	maxGrepFiles   = 20
	gitTimeout     = 10 * time.Second
	hashPrefixBlob = "blob:"
	hashPrefixSym  = "sym:"
)

// Repo is a git work tree whose HEAD commit references are checked against.
type Repo struct {
	root string
	head string
}

// ErrNoCommits is returned by Open for a repository without commits.
var ErrNoCommits = errors.New("repository has no commits")

// Open returns the repository containing dir.
func Open(ctx context.Context, dir string) (*Repo, error) {
	root, err := git(ctx, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	r := &Repo{root: strings.TrimSpace(string(root))}
	head, err := git(ctx, r.root, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		return nil, ErrNoCommits
	}
	r.head = strings.TrimSpace(string(head))
	return r, nil
}

// Head returns the commit references are checked at.
func (r *Repo) Head() string { return r.head }

// Root returns the work tree's top-level directory.
func (r *Repo) Root() string { return r.root }

// Check computes the state of each target at HEAD. Targets are skipped (no
// result) when checking them could give a wrong answer:
//
//   - anchored at a commit HEAD does not contain (an older checkout, or
//     another branch), since the difference may be history, not a change;
//   - not anchored yet while their file has uncommitted changes, since the
//     memory may describe the uncommitted code;
//   - anchored symbols in a file this build cannot parse, or that did not
//     parse cleanly.
//
// A pending symbol that cannot be looked up here is reported as not found,
// which leaves it unresolved until a client that can parse its file anchors
// it. When ctx is done, Check stops and returns the results it has.
func (r *Repo) Check(ctx context.Context, targets []Target) ([]Result, error) {
	if len(targets) > maxTargets {
		targets = targets[:maxTargets]
	}
	var valid []Target
	for _, t := range targets {
		if ValidTarget(t) {
			valid = append(valid, t)
		}
	}
	ancestors := map[string]bool{}
	for _, t := range valid {
		if c := t.AnchorCommit; c != "" {
			if _, done := ancestors[c]; !done {
				ancestors[c] = c == r.head || r.isAncestor(ctx, c)
			}
		}
	}
	dirty, err := r.dirtyPaths(ctx)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, t := range valid {
		if t.Path != "" && !slices.Contains(files, t.Path) {
			files = append(files, t.Path)
		}
	}
	blobs, err := r.blobs(ctx, files)
	if err != nil {
		return nil, err
	}
	c := &checker{repo: r, dirty: dirty, blobs: blobs, contents: map[string][]byte{}}

	var out []Result
	for _, t := range valid {
		if ctx.Err() != nil {
			break
		}
		if t.AnchorCommit != "" && !ancestors[t.AnchorCommit] {
			continue
		}
		res, ok, err := c.check(ctx, t)
		if err != nil {
			if ctx.Err() != nil {
				break // out of time: report what is done
			}
			return out, err
		}
		if ok {
			out = append(out, res)
		}
	}
	return out, nil
}

// checker holds the per-Check state.
type checker struct {
	repo     *Repo
	dirty    map[string]bool
	blobs    map[string]string // path -> blob id at HEAD
	contents map[string][]byte
}

// check computes one target's result; ok is false when it must be skipped.
func (c *checker) check(ctx context.Context, t Target) (res Result, ok bool, err error) {
	pending := t.AnchorCommit == ""
	res = Result{Path: t.Path, Symbol: t.Symbol, AnchorCommit: t.AnchorCommit}
	switch {
	case t.Symbol == "":
		if pending && c.dirty[t.Path] {
			return res, false, nil
		}
		if sha, found := c.blobs[t.Path]; found {
			res.Found, res.Hash = true, hashPrefixBlob+sha
		}
		return res, true, nil

	case t.Path != "":
		if pending && c.dirty[t.Path] {
			return res, false, nil
		}
		if _, found := c.blobs[t.Path]; !found {
			return res, true, nil // no such file: missing, or unresolved if pending
		}
		if !parsable(t.Path) {
			return res, pending, nil
		}
		src, err := c.repo.content(ctx, c.contents, t.Path)
		if err != nil {
			return res, false, err
		}
		defs, err := definitions(t.Path, src, t.Symbol)
		switch {
		case errors.Is(err, errUnsupported):
			return res, pending, nil // e.g. too large to parse
		case err != nil:
			return res, false, nil // did not parse cleanly: leave it as it is
		}
		if len(defs) > 0 {
			res.Found, res.Hash = true, symbolHash([]string{t.Path}, [][]string{defs})
		}
		return res, true, nil

	default: // symbol without a file: search the repository
		paths, defs, complete, err := c.repo.findDefinitions(ctx, c.contents, t.Symbol)
		if err != nil {
			return res, false, err
		}
		if !complete {
			// Some code file that mentions the name could not be read, so
			// the definitions (and their hash) may be partial: a client that
			// can read them all decides. A pending symbol found nowhere
			// readable is reported unresolved, which lets that client
			// anchor it later.
			return res, pending && len(paths) == 0, nil
		}
		if pending && slices.ContainsFunc(paths, func(p string) bool { return c.dirty[p] }) {
			return res, false, nil
		}
		if len(paths) > 0 {
			res.Found, res.Hash, res.ResolvedPath = true, symbolHash(paths, defs), paths[0]
		}
		return res, true, nil
	}
}

// findDefinitions finds the files at HEAD that define symbol. complete is
// false when some file that mentions the name could not be parsed, so the
// answer may be incomplete.
func (r *Repo) findDefinitions(ctx context.Context, contents map[string][]byte, symbol string) (paths []string, defs [][]string, complete bool, err error) {
	_, name := splitSymbol(symbol)
	outb, err := git(ctx, r.root, nil, "grep", "-l", "-w", "-F", "-I", "--full-name", "-e", name, r.head, "--")
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 && ctx.Err() == nil {
			return nil, nil, true, nil // no file mentions it: it is not defined anywhere
		}
		return nil, nil, false, err
	}
	complete = true
	var candidates []string
	for _, line := range strings.Split(strings.TrimSpace(string(outb)), "\n") {
		p := strings.TrimPrefix(line, r.head+":")
		if p == "" || p == line {
			continue
		}
		if len(candidates) == maxGrepFiles {
			complete = false
			break
		}
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		if !parsable(p) {
			if codeFile(p) {
				complete = false // code this build cannot parse may define it
			}
			continue // a mention in docs or config
		}
		src, err := r.content(ctx, contents, p)
		if err != nil {
			return nil, nil, false, err
		}
		d, err := definitions(p, src, symbol)
		if err != nil {
			complete = false
			continue
		}
		if len(d) > 0 {
			paths, defs = append(paths, p), append(defs, d)
		}
	}
	return paths, defs, complete, nil
}

// symbolHash hashes definitions (per file, in file order) ignoring
// whitespace, so reformatting does not count as a change.
func symbolHash(paths []string, defs [][]string) string {
	h := sha256.New()
	for i, p := range paths {
		io.WriteString(h, p+"\x00")
		for _, d := range defs[i] {
			io.WriteString(h, strings.Join(strings.Fields(d), " ")+"\x00")
		}
	}
	return hashPrefixSym + hex.EncodeToString(h.Sum(nil)[:16])
}

func (r *Repo) isAncestor(ctx context.Context, commit string) bool {
	_, err := git(ctx, r.root, nil, "merge-base", "--is-ancestor", commit, r.head)
	return err == nil
}

// dirtyPaths returns tracked files with uncommitted changes.
func (r *Repo) dirtyPaths(ctx context.Context) (map[string]bool, error) {
	outb, err := git(ctx, r.root, nil, "status", "--porcelain=v1", "-z", "--untracked-files=no")
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	dirty := map[string]bool{}
	entries := strings.Split(string(outb), "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		dirty[e[3:]] = true
		if e[0] == 'R' || e[0] == 'C' { // renames and copies are followed by the source path
			i++
			if i < len(entries) {
				dirty[entries[i]] = true
			}
		}
	}
	return dirty, nil
}

// blobs returns the blob id at HEAD of each path that is a file there.
func (r *Repo) blobs(ctx context.Context, paths []string) (map[string]string, error) {
	out := map[string]string{}
	for start := 0; start < len(paths); start += 200 {
		batch := paths[start:min(start+200, len(paths))]
		outb, err := git(ctx, r.root, nil, append([]string{"ls-tree", "-z", "--full-tree", r.head, "--"}, batch...)...)
		if err != nil {
			return nil, fmt.Errorf("git ls-tree: %w", err)
		}
		for _, e := range strings.Split(string(outb), "\x00") {
			// <mode> SP <type> SP <object> TAB <file>
			meta, file, ok := strings.Cut(e, "\t")
			if !ok {
				continue
			}
			if f := strings.Fields(meta); len(f) == 3 && f[1] == "blob" {
				out[file] = f[2]
			}
		}
	}
	return out, nil
}

// content returns a file's content at HEAD (cached in contents).
func (r *Repo) content(ctx context.Context, contents map[string][]byte, p string) ([]byte, error) {
	if b, ok := contents[p]; ok {
		return b, nil
	}
	outb, err := git(ctx, r.root, strings.NewReader(r.head+":"+p+"\n"), "cat-file", "--batch")
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	br := bufio.NewReader(bytes.NewReader(outb))
	header, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	f := strings.Fields(header)
	var b []byte
	if len(f) == 3 && f[1] == "blob" {
		size, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, fmt.Errorf("git cat-file: bad size %q", f[2])
		}
		if size <= maxParseBytes {
			b = make([]byte, size)
			if _, err := io.ReadFull(br, b); err != nil {
				return nil, fmt.Errorf("git cat-file: %w", err)
			}
		} else {
			b = make([]byte, maxParseBytes+1) // too big to parse; definitions reports unsupported
		}
	}
	contents[p] = b
	return b, nil
}

// ValidTarget reports whether t is well formed: a repository-relative path
// without "..", an identifier-like symbol, and a hexadecimal anchor commit.
// Clients check it before anything reaches git's command line.
func ValidTarget(t Target) bool {
	if t.Path == "" && t.Symbol == "" {
		return false
	}
	if t.Path != "" {
		if p, ok := cleanPath(t.Path); !ok || p != t.Path {
			return false
		}
	}
	if t.Symbol != "" && (len(t.Symbol) > maxSymbolLen || !symbolRE.MatchString(t.Symbol)) {
		return false
	}
	if c := t.AnchorCommit; c != "" && !ValidCommit(c) {
		return false
	}
	return true
}

// ValidCommit reports whether s looks like a commit id (7–64 hex digits).
func ValidCommit(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	_, err := hex.DecodeString(s + strings.Repeat("0", len(s)%2))
	return err == nil
}

// git runs git in dir without prompts or optional locks.
func git(ctx context.Context, dir string, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out, err
		}
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
