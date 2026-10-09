package bench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kenfold/kenfold/internal/chat"
)

func scoreChildEnv() []string {
	var env []string
	for _, name := range []string{"HOME", "PATH", "TMPDIR", "KENFOLD_SCORE_TEST_DATABASE_URL", "PGPASSFILE"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}

func TestScoreCheckpointRequiresPrivatePinnedDirectory(t *testing.T) {
	for _, kind := range []string{"empty-dir", "empty-identity", "public-dir", "symlink-dir", "public-lock", "symlink-lock", "public-namespace", "corrupt-namespace"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "scores")
			identity := "fixture-v1"
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "empty-dir":
				dir = ""
			case "empty-identity":
				identity = " \n"
			case "public-dir":
				if err := os.Chmod(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink-dir":
				link := dir + "-link"
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				dir = link
			case "public-lock", "symlink-lock":
				path := filepath.Join(dir, ".score.lock")
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if kind == "public-lock" {
					if err := os.Chmod(path, 0o644); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Rename(path, path+"-target"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(path+"-target", path); err != nil {
						t.Fatal(err)
					}
				}
			case "public-namespace", "corrupt-namespace":
				path := filepath.Join(dir, "score-cache.json")
				mode := os.FileMode(0o600)
				if kind == "public-namespace" {
					mode = 0o644
				}
				if err := os.WriteFile(path, []byte("{"), mode); err != nil {
					t.Fatal(err)
				}
			}
			c, err := NewScoreCache(dir, identity)
			if c != nil {
				c.Close()
			}
			if err == nil {
				t.Fatal("unsafe/unpinned scoring cache was accepted")
			}
		})
	}
}

func TestScoreCheckpointExclusiveLifetimeLock(t *testing.T) {
	if dir := os.Getenv("KENFOLD_SCORE_LOCK_CHILD"); dir != "" {
		c, err := NewScoreCache(dir, "fixture-v1")
		if c != nil {
			c.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "locked") {
			t.Fatalf("child bypassed writer lock: %v", err)
		}
		return
	}
	dir := filepath.Join(t.TempDir(), "scores")
	c, err := NewScoreCache(dir, "fixture-v1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if other, err := NewScoreCache(dir, "fixture-v1"); err == nil {
		other.Close()
		t.Fatal("two writers acquired the same scoring cache")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestScoreCheckpointExclusiveLifetimeLock$")
	cmd.Env = append(scoreChildEnv(), "KENFOLD_SCORE_LOCK_CHILD="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("process lock failed: %v\n%s", err, out)
	}
	lockPath := filepath.Join(dir, ".score.lock")
	before, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(lockPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("Close unlinked or replaced its lifetime lock")
	}
	c, err = NewScoreCache(dir, "fixture-v1")
	if err != nil {
		t.Fatalf("closed process lock was not released: %v", err)
	}
	defer c.Close()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := NewScoreCache(dir, "different-fixture-identity"); err == nil {
		other.Close()
		t.Fatal("an incompatible namespace silently became a fresh scoring cache")
	}
}

func scoreQuestionRecordPath(t *testing.T, dir string) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "question-*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("want one durable question record, got %d: %v", len(paths), err)
	}
	return paths[0]
}

func TestScoreCheckpointPersistsOnlyHashedEndpointAndIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scores")
	identity := "fixture-sensitive-pinned-profile-marker"
	c, err := NewScoreCache(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	endpoint := "http://127.0.0.1:1/fixture?token=fixture-sensitive-url-marker"
	client, err := chat.New(chat.Config{BaseURL: endpoint, Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	key := c.key(scoredQ{Scope: "project:fixture", Question: Question{ID: "q", Type: "2", Text: "Color?", Answer: "blue"}}, true, client, client)
	if err := c.save(scoreState{Key: key, Phase: scoreReaderInFlight}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "score-cache.json"), filepath.Join(dir, ".score.lock"), scoreQuestionRecordPath(t, dir)} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("cache file is not owner-only: %v", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), identity) || strings.Contains(string(data), "fixture-sensitive-url-marker") || strings.Contains(string(data), "http://") {
			t.Fatal("checkpoint persisted raw endpoint or identity")
		}
	}
}
