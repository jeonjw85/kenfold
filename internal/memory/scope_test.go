package memory

import (
	"strings"
	"testing"
)

func TestScope(t *testing.T) {
	for in, want := range map[string]string{
		"":                                       ScopeUser,
		"  ":                                     ScopeUser,
		"github.com/kenfold/kenfold":             "project:github.com/kenfold/kenfold",
		"https://github.com/Kenfold/Kenfold.git": "project:github.com/kenfold/kenfold",
		"https://github.com/o/r/":                "project:github.com/o/r",
		"git@github.com:o/r.git":                 "project:github.com/o/r",
		"github.com:o/r":                         "project:github.com/o/r",
		"ssh://git@example.com/o/r/":             "project:example.com/o/r",
		"ssh://git@host.example:2222/o/r.git":    "project:host.example/o/r",
		"https://user:ghp_secret@github.com/o/r": "project:github.com/o/r",
		"https://github.com/o/r?tab=readme#top":  "project:github.com/o/r",
		"My-Local-Project":                       "project:my-local-project",
	} {
		got, err := Scope(in)
		if err != nil || got != want {
			t.Errorf("Scope(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"https://", ".git", "/", strings.Repeat("a", maxProjectRunes+1), "a\nb", "\x00bad",
		"/Users/me/dev/kenfold", "~/dev/kenfold", "./kenfold", "../kenfold", "file:///srv/git/repo.git", `C:\src\repo`, "c:/src/repo", `\\server\share\repo`} {
		if got, err := Scope(bad); err == nil {
			t.Errorf("Scope(%q) = %q; want error", bad, got)
		}
	}
	if _, err := Scope("/Users/me/dev/kenfold"); err == nil || !strings.Contains(err.Error(), "git remote URL") {
		t.Errorf("local path error should explain what to pass: %v", err)
	}
}

func TestNormalizeProjectIsIdempotent(t *testing.T) {
	for _, in := range []string{"https://github.com/O/R.git", "git@gitlab.com:group/sub/repo.git", "plain-name"} {
		once := NormalizeProject(in)
		if twice := NormalizeProject(once); twice != once {
			t.Errorf("NormalizeProject not idempotent for %q: %q -> %q", in, once, twice)
		}
	}
}
