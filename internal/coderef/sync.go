package coderef

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Wire types of the REST API (GET /api/v1/refs, POST /api/v1/refs/check).

// TargetList is the response of GET /api/v1/refs?project=P.
type TargetList struct {
	Scope   string   `json:"scope"`
	Targets []Target `json:"targets"`
}

// CheckReport is the body of POST /api/v1/refs/check.
type CheckReport struct {
	Project string   `json:"project"`
	Commit  string   `json:"commit"`
	Results []Result `json:"results"`
}

// CheckSummary is the response of POST /api/v1/refs/check.
type CheckSummary struct {
	Updated   int            `json:"updated"`
	Anchored  int            `json:"anchored"`
	Unmatched int            `json:"unmatched"`
	States    map[string]int `json:"states,omitempty"`
}

// SyncOptions configures Sync.
type SyncOptions struct {
	// APIBase is the REST API root, e.g. http://127.0.0.1:7077/api/v1.
	APIBase string
	// APIKey is sent as a bearer token.
	APIKey string
	// Dir is a directory inside the repository.
	Dir string
	// Project is the repository's project identity (normalized remote URL).
	Project    string
	HTTPClient *http.Client
}

// SyncResult reports what Sync did.
type SyncResult struct {
	Commit  string
	Targets int // references the server listed
	Checked int // results sent
	Summary CheckSummary
}

// APIBase derives the REST API root from the MCP endpoint URL
// (http://host:7077/mcp -> http://host:7077/api/v1).
func APIBase(mcpURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(mcpURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid Kenfold URL %q", mcpURL)
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/mcp") + "/api/v1"
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// Sync checks the project's code references against the repository at Dir
// and reports the results to the server: fetch targets, hash them at HEAD,
// send the results. It returns early without error when the server lists no
// references.
func Sync(ctx context.Context, o SyncOptions) (SyncResult, error) {
	repo, err := Open(ctx, o.Dir)
	if err != nil {
		return SyncResult{}, err
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	res := SyncResult{Commit: repo.Head()}

	var list TargetList
	q := url.Values{"project": {o.Project}}
	if err := call(ctx, hc, http.MethodGet, o.APIBase+"/refs?"+q.Encode(), o.APIKey, nil, &list); err != nil {
		return res, err
	}
	res.Targets = len(list.Targets)
	if len(list.Targets) == 0 {
		return res, nil
	}
	// Leave a quarter of the remaining time for reporting, so work done
	// before a deadline is not lost.
	checkCtx := ctx
	if dl, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		checkCtx, cancel = context.WithTimeout(ctx, time.Until(dl)*3/4)
		defer cancel()
	}
	results, err := repo.Check(checkCtx, list.Targets)
	if err != nil {
		return res, err
	}
	res.Checked = len(results)
	if len(results) == 0 {
		return res, nil
	}
	err = call(ctx, hc, http.MethodPost, o.APIBase+"/refs/check", o.APIKey,
		CheckReport{Project: o.Project, Commit: repo.Head(), Results: results}, &res.Summary)
	return res, err
}

// StatusError is a non-2xx response from the REST API.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("kenfold API: %d %s", e.Code, e.Msg) }

func call(ctx context.Context, hc *http.Client, method, u, key string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return &StatusError{Code: resp.StatusCode, Msg: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("kenfold API: decode response: %w", err)
	}
	return nil
}
