// Package restapi serves Kenfold's REST API for clients that are not agents:
// today, code reference sync for the session hook and `kenfold refs sync`.
// It is mounted under /api/v1 behind the same Host check, cross-origin
// protection, and API key authentication as /mcp.
package restapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/kenfold/kenfold/internal/authz"
	"github.com/kenfold/kenfold/internal/coderef"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/store"
)

// Prefix is where the API is mounted.
const Prefix = "/api/v1/"

const (
	maxBody    = 1 << 20
	maxResults = 1000
	maxHash    = 100
)

type api struct {
	store *store.Store
	log   *slog.Logger
}

// New returns the API handler (paths include Prefix).
func New(st *store.Store, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	a := &api{store: st, log: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+Prefix+"refs", a.listRefs)
	mux.HandleFunc("POST "+Prefix+"refs/check", a.checkRefs)
	// Method-qualified patterns win over these, which answer other methods.
	mux.HandleFunc(Prefix+"refs", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc(Prefix+"refs/check", methodNotAllowed("POST"))
	mux.HandleFunc(Prefix, func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	return mux
}

func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use "+allow)
	}
}

// projectScope maps a project to its scope; user-wide is not a repository.
func projectScope(project string) (string, error) {
	if strings.TrimSpace(project) == "" {
		return "", errors.New("project is required")
	}
	scope, err := memory.Scope(project)
	if err != nil {
		return "", err
	}
	if scope == memory.ScopeUser {
		return "", errors.New("project is required")
	}
	return scope, nil
}

func (a *api) listRefs(w http.ResponseWriter, r *http.Request) {
	scope, err := projectScope(r.URL.Query().Get("project"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	targets, err := a.store.SyncTargets(r.Context(), scope)
	if err != nil {
		a.internal(w, r, "list refs", err)
		return
	}
	out := coderef.TargetList{Scope: scope, Targets: make([]coderef.Target, 0, len(targets))}
	for _, t := range targets {
		out.Targets = append(out.Targets, coderef.Target{Path: t.Path, Symbol: t.Symbol, AnchorCommit: t.AnchorCommit, AnchorHash: t.AnchorHash})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) checkRefs(w http.ResponseWriter, r *http.Request) {
	if !authz.CanWrite(auth.TokenInfoFromContext(r.Context())) {
		writeError(w, http.StatusForbidden, "this client was granted read-only access")
		return
	}
	var rep coderef.CheckReport
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		writeError(w, http.StatusBadRequest, "body must be a JSON check report")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "body must contain exactly one JSON check report")
		return
	}
	scope, err := projectScope(rep.Project)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !coderef.ValidCommit(rep.Commit) {
		writeError(w, http.StatusBadRequest, "commit must be a hexadecimal commit id")
		return
	}
	if len(rep.Results) > maxResults {
		writeError(w, http.StatusBadRequest, "too many results")
		return
	}
	checks := make([]store.RefCheck, 0, len(rep.Results))
	for i, res := range rep.Results {
		if msg := validResult(res); msg != "" {
			writeError(w, http.StatusBadRequest, "results["+strconv.Itoa(i)+"]: "+msg)
			return
		}
		checks = append(checks, store.RefCheck{Path: res.Path, Symbol: res.Symbol, AnchorCommit: res.AnchorCommit,
			Found: res.Found, Hash: res.Hash, ResolvedPath: res.ResolvedPath})
	}
	sum, err := a.store.ApplyRefChecks(r.Context(), scope, rep.Commit, checks)
	if err != nil {
		a.internal(w, r, "check refs", err)
		return
	}
	a.log.InfoContext(r.Context(), "code references checked", "scope", scope, "commit", rep.Commit[:min(12, len(rep.Commit))],
		"results", len(checks), "updated", sum.Updated, "anchored", sum.Anchored, "states", sum.States)
	writeJSON(w, http.StatusOK, coderef.CheckSummary{Updated: sum.Updated, Anchored: sum.Anchored, Unmatched: sum.Unmatched, States: sum.States})
}

// validResult returns why res is unacceptable, or "".
func validResult(res coderef.Result) string {
	t := coderef.Target{Path: res.Path, Symbol: res.Symbol, AnchorCommit: res.AnchorCommit}
	if !coderef.ValidTarget(t) {
		return "invalid path, symbol, or anchor commit"
	}
	if res.Found && (res.Hash == "" || len(res.Hash) > maxHash || !utf8.ValidString(res.Hash)) {
		return "found results need a hash of at most 100 characters"
	}
	if !res.Found && res.Hash != "" {
		return "results that were not found have no hash"
	}
	if p := res.ResolvedPath; p != "" && !coderef.ValidTarget(coderef.Target{Path: p}) {
		return "invalid resolved_path"
	}
	return ""
}

func (a *api) internal(w http.ResponseWriter, r *http.Request, op string, err error) {
	a.log.ErrorContext(r.Context(), "api request failed", "op", op, "err", err)
	writeError(w, http.StatusInternalServerError, op+" failed because of an internal error; the Kenfold server log has details")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
