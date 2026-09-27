package memory

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ScopeUser is the scope of user-wide memories.
const ScopeUser = "user"

// maxProjectRunes bounds a normalized project identity.
const maxProjectRunes = 256

// Scope maps a project argument (git remote URL or project name) to a memory
// scope: "user" when project is empty, otherwise "project:<normalized>".
// Filesystem paths are rejected: they differ per machine and checkout, so the
// same repository would split into several projects.
func Scope(project string) (string, error) {
	if strings.TrimSpace(project) == "" {
		return ScopeUser, nil
	}
	if isLocalPath(project) {
		return "", fmt.Errorf("project %q is a local path; pass the repository's git remote URL (git remote get-url origin), or a project name if it has no remote", strings.TrimSpace(project))
	}
	p := NormalizeProject(project)
	if p == "" {
		return "", fmt.Errorf("project %q is not a git remote URL or project name", project)
	}
	if utf8.RuneCountInString(p) > maxProjectRunes {
		return "", fmt.Errorf("project is longer than %d characters", maxProjectRunes)
	}
	if strings.ContainsFunc(p, unicode.IsControl) {
		return "", errors.New("project contains control characters")
	}
	return "project:" + p, nil
}

// isLocalPath reports whether p is a filesystem path (absolute, home-relative,
// dot-relative, a file:// URL, or a Windows drive path) rather than a remote.
func isLocalPath(p string) bool {
	p = strings.TrimSpace(p)
	switch {
	case strings.HasPrefix(p, "/"), strings.HasPrefix(p, "~"), strings.HasPrefix(p, "./"), strings.HasPrefix(p, "../"),
		strings.HasPrefix(strings.ToLower(p), "file://"), strings.HasPrefix(p, `\\`):
		return true
	}
	// C:\ or C:/
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		(p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z')
}

// NormalizeProject reduces a git remote URL to a stable, lowercase host/path
// identity so the same repository maps to the same project across agents and
// machines (ADR-0001). It strips the scheme, credentials, port, query, and
// ".git":
//
//	https://github.com/Owner/Repo.git      -> github.com/owner/repo
//	git@github.com:owner/repo.git          -> github.com/owner/repo
//	ssh://git@host:2222/owner/repo         -> host/owner/repo
//	https://user:token@github.com/o/r      -> github.com/o/r
//
// Anything else (a plain project name) is lowercased and trimmed.
func NormalizeProject(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if _, rest, ok := strings.Cut(p, "://"); ok {
		p = rest
		if i := strings.IndexAny(p, "?#"); i >= 0 {
			p = p[:i]
		}
	} else if host, path, ok := strings.Cut(p, ":"); ok && host != "" && path != "" && !strings.Contains(host, "/") {
		p = host + "/" + path // scp-like: [user@]host:path
	}
	// Credentials: user[:password]@host/...
	if at := strings.IndexByte(p, '@'); at >= 0 {
		if slash := strings.IndexByte(p, '/'); slash < 0 || at < slash {
			p = p[at+1:]
		}
	}
	// Port: host:1234/path
	if host, path, ok := strings.Cut(p, "/"); ok {
		if h, port, ok := strings.Cut(host, ":"); ok && port != "" && strings.Trim(port, "0123456789") == "" {
			p = h + "/" + path
		}
	}
	p = strings.TrimRight(p, "/")
	p = strings.TrimSuffix(p, ".git")
	return strings.TrimRight(p, "/")
}
