// Package update reports a newer GitHub release. It does not download or restart.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Interval is how often a running server repeats the check.
	Interval = 12 * time.Hour
	// Timeout bounds one check.
	Timeout = 4 * time.Second

	maxBody = 1 << 20
)

// Endpoint is the latest-release document. Tests may replace it.
var Endpoint = "https://api.github.com/repos/jeonjw85/kenfold/releases/latest"

// Release is a published release newer than the running build.
type Release struct {
	Version string
	URL     string
}

// Check reports a newer release than current. current must be vMAJOR.MINOR.PATCH;
// otherwise Check does not use the network. Redirects are not followed.
// A zero Release means there is nothing to announce.
func Check(ctx context.Context, client *http.Client, endpoint, current string) (Release, error) {
	if !IsRelease(current) {
		return Release{}, nil
	}
	hc := &http.Client{Timeout: Timeout}
	if client != nil {
		copied := *client
		hc = &copied
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("User-Agent", "kenfold")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := hc.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("update check: %s", resp.Status)
	}
	var doc struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&doc); err != nil {
		return Release{}, err
	}
	if doc.Draft || doc.Prerelease || !newer(current, doc.TagName) {
		return Release{}, nil
	}
	return Release{Version: doc.TagName, URL: releaseURL(doc.TagName)}, nil
}

// IsRelease reports whether v is exactly vMAJOR.MINOR.PATCH.
func IsRelease(v string) bool {
	_, ok := parseRelease(v)
	return ok
}

func releaseURL(tag string) string {
	return "https://github.com/jeonjw85/kenfold/releases/tag/" + tag
}

func newer(current, latest string) bool {
	c, okC := parseRelease(current)
	l, okL := parseRelease(latest)
	if !okC || !okL {
		return false
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parseRelease(s string) ([3]int, bool) {
	rest, ok := strings.CutPrefix(s, "v")
	if !ok {
		return [3]int{}, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var n [3]int
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') || strings.Trim(p, "0123456789") != "" {
			return [3]int{}, false
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, false
		}
		n[i] = v
	}
	return n, true
}

// Cache holds the last announced release for the dashboard.
type Cache struct {
	mu        sync.Mutex
	cur       Release
	announced string
}

// Store records r. It reports whether this version should be logged.
func (c *Cache) Store(r Release) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = r
	if r.Version == "" || r.Version == c.announced {
		return false
	}
	c.announced = r.Version
	return true
}

// Get returns the release to show, or a zero Release.
func (c *Cache) Get() Release {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}
