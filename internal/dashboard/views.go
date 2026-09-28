package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kenfold/kenfold/internal/apikey"
	"github.com/kenfold/kenfold/internal/memory"
	"github.com/kenfold/kenfold/internal/oauth"
	"github.com/kenfold/kenfold/internal/retrieve"
	"github.com/kenfold/kenfold/internal/store"
)

// Proposal is a consolidation proposal as the dashboard shows it.
type Proposal struct {
	ID        string
	Kind      string // "merge", "conflict", or "digest"
	Scope     string
	Content   string // the merged statement or digest ("" for conflicts)
	Keep      string // conflicts: the id of the memory to keep
	Reason    string
	Model     string
	Members   []store.Memory
	CreatedAt time.Time
}

// Consolidation lists and decides consolidation proposals.
type Consolidation interface {
	Pending(ctx context.Context, limit int) ([]Proposal, error)
	PendingCount(ctx context.Context) (int, error)
	Apply(ctx context.Context, id, agent string) (string, error) // returns a summary of what changed
	Reject(ctx context.Context, id, agent string) error
}

// view is what every page template receives.
type view struct {
	Title    string
	Nav      string
	Flash    string
	Error    string
	CSRF     string
	Path     string
	Version  string
	LoggedIn bool
	Pending  int
	Consol   bool // consolidation is enabled
	Data     any
}

var funcs = template.FuncMap{
	"ts": func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	},
	"tsp": func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return "-"
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	},
	"short": func(id string) string { return id[:min(8, len(id))] },
	"trunc": func(s string, n int) string {
		s = strings.Join(strings.Fields(s), " ")
		if utf8.RuneCountInString(s) <= n {
			return s
		}
		return string([]rune(s)[:n-1]) + "…"
	},
	"attr": func(attrs map[string]any, k string) string {
		switch v := attrs[k].(type) {
		case nil:
			return ""
		case string:
			return v
		default:
			b, _ := json.Marshal(v)
			return string(b)
		}
	},
	"pretty": func(v any) string {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	},
	"deref": func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	},
	"stale": func(refs []store.CodeRef) bool { return slices.ContainsFunc(refs, store.CodeRef.Stale) },
	"join":  strings.Join,
	"add":   func(a, b int) int { return a + b },
	"pct":   func(f float64) string { return strconv.Itoa(int(f*100+0.5)) + "%" },
	"scopeLabel": func(s string) string {
		if s == memory.ScopeUser {
			return "user-wide"
		}
		return strings.TrimPrefix(strings.TrimPrefix(s, "project:"), "repo:")
	},
}

func parsePages() (map[string]*template.Template, error) {
	names, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*template.Template{}
	for _, n := range names {
		base := path.Base(n)
		if base == "layout.html" {
			continue
		}
		t, err := template.New(base).Funcs(funcs).ParseFS(assets, "templates/layout.html", n)
		if err != nil {
			return nil, fmt.Errorf("dashboard template %s: %w", base, err)
		}
		pages[base] = t
	}
	return pages, nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, v view) {
	t := s.pages[page]
	if sess := sessionOf(r.Context()); sess != nil {
		v.CSRF, v.LoggedIn = sess.csrf, true
		v.Flash = s.sessions.takeFlash(sess)
		v.Pending = s.pendingCount(r.Context())
	}
	v.Version, v.Path, v.Consol = s.d.Version, r.URL.RequestURI(), s.d.Consolidation != nil
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", v); err != nil {
		s.log.ErrorContext(r.Context(), "dashboard template failed", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *Server) pendingCount(ctx context.Context) int {
	n, _ := s.d.Store.CountStatus(ctx, memory.StatusProposed)
	if s.d.Consolidation != nil {
		c, _ := s.d.Consolidation.PendingCount(ctx)
		n += c
	}
	return n
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "message.html", view{Title: "Not found", Error: "There is nothing at this address."})
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.ErrorContext(r.Context(), "dashboard request failed", "path", r.URL.Path, "err", err)
	s.render(w, r, http.StatusInternalServerError, "message.html", view{Title: "Error", Error: "Something went wrong. The Kenfold server log has details."})
}

func pageParam(r *http.Request) int {
	p, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || p < 1 {
		return 1
	}
	return min(p, 1000)
}

// scopeParam validates a scope filter ("" = all scopes).
func scopeParam(v string) string {
	v = strings.TrimSpace(v)
	if v == memory.ScopeUser || strings.HasPrefix(v, "project:") || strings.HasPrefix(v, "repo:") {
		return v
	}
	return ""
}

// ---- overview ----

type typeCount struct {
	Type  memory.Type
	Count int
}

type overviewData struct {
	Proposed, Superseded, Deleted, Stale, Active int
	ByType                                       []typeCount
	Scopes                                       []store.ScopeCount
	Extraction                                   store.ExtractionStats
	Keys, Grants, Proposals                      int
	HasConsol                                    bool
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.d.Store.Stats(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := overviewData{
		Proposed: st.ByStatus[memory.StatusProposed], Superseded: st.ByStatus[memory.StatusSuperseded], Deleted: st.ByStatus[memory.StatusDeleted],
		Stale: st.Stale, Scopes: st.Scopes, HasConsol: s.d.Consolidation != nil,
	}
	for _, t := range memory.Types {
		d.ByType = append(d.ByType, typeCount{Type: t, Count: st.ActiveType[t]})
		d.Active += st.ActiveType[t]
	}
	d.Extraction, _ = s.d.Store.ExtractionStatus(ctx)
	if keys, err := s.d.Keys.List(ctx, false); err == nil {
		d.Keys = len(keys)
	}
	if gs, err := s.d.Owner.Grants(ctx, false); err == nil {
		d.Grants = len(gs)
	}
	if s.d.Consolidation != nil {
		d.Proposals, _ = s.d.Consolidation.PendingCount(ctx)
	}
	s.render(w, r, http.StatusOK, "overview.html", view{Title: "Overview", Nav: "overview", Data: d})
}

// ---- review ----

type reviewItem struct {
	Memory  store.Memory
	Similar []store.Memory
}

type reviewData struct {
	Items  []reviewItem
	Page   int
	More   bool
	Scope  string
	Scopes []store.ScopeCount
}

func (s *Server) review(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page, scope := pageParam(r), scopeParam(r.URL.Query().Get("scope"))
	p := store.ListParams{Statuses: []memory.Status{memory.StatusProposed}, OldestFirst: true, Limit: reviewPageSize + 1, Offset: (page - 1) * reviewPageSize}
	if scope != "" {
		p.Scopes = []string{scope}
	}
	ms, err := s.d.Store.List(ctx, p)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := reviewData{Page: page, Scope: scope, More: len(ms) > reviewPageSize}
	if st, err := s.d.Store.Stats(ctx); err == nil {
		for _, sc := range st.Scopes {
			if sc.Proposed > 0 {
				d.Scopes = append(d.Scopes, sc)
			}
		}
	}
	for _, m := range ms[:min(len(ms), reviewPageSize)] {
		d.Items = append(d.Items, reviewItem{Memory: m, Similar: s.activeSimilar(ctx, m)})
	}
	s.render(w, r, http.StatusOK, "review.html", view{Title: "Review", Nav: "review", Data: d})
}

// activeSimilar returns active memories a proposed one may duplicate or
// contradict: those recorded when it was extracted, and those similar now.
func (s *Server) activeSimilar(ctx context.Context, m store.Memory) []store.Memory {
	var out []store.Memory
	seen := map[string]bool{m.ID: true}
	add := func(x store.Memory) {
		if !seen[x.ID] && x.Status == memory.StatusActive && x.Scope == m.Scope && len(out) < 4 {
			seen[x.ID] = true
			out = append(out, x)
		}
	}
	if raw, ok := m.Attrs["similar_to"].([]any); ok {
		for _, v := range raw {
			if id, ok := v.(string); ok && store.ValidID(id) {
				if x, err := s.d.Store.Get(ctx, id); err == nil {
					add(x)
				}
			}
		}
	}
	if res, err := s.d.Store.Similar(ctx, store.SimilarParams{Scope: m.Scope, Content: m.Content, Exclude: m.ID}); err == nil {
		for _, x := range res {
			add(x.Memory)
		}
	}
	return out
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	_, err := s.d.Store.Approve(r.Context(), r.PathValue("id"))
	s.decided(w, r, err, "Approved: the memory is now served to your agents.")
}

func (s *Server) approveReplacing(w http.ResponseWriter, r *http.Request) {
	_, err := s.d.Store.ApproveReplacing(r.Context(), r.PathValue("id"), r.PostForm.Get("target"))
	s.decided(w, r, err, "Approved; the memory it replaces is kept as history.")
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request) {
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if reason == "" {
		reason = "rejected in the dashboard"
	}
	if utf8.RuneCountInString(reason) > 500 {
		reason = string([]rune(reason)[:500])
	}
	_, err := s.d.Store.Reject(r.Context(), r.PathValue("id"), reason, Agent)
	s.decided(w, r, err, "Rejected: it will not be proposed again.")
}

func (s *Server) decided(w http.ResponseWriter, r *http.Request, err error, ok string) {
	switch {
	case err == nil:
		s.done(w, r, ok)
	case errors.Is(err, store.ErrNotProposed):
		s.done(w, r, "That memory was already decided.")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrInvalidID):
		s.done(w, r, "That memory no longer exists.")
	case errors.Is(err, store.ErrNotActive):
		s.done(w, r, "The memory to replace is no longer active; nothing was changed.")
	default:
		s.log.ErrorContext(r.Context(), "dashboard action failed", "path", r.URL.Path, "err", err)
		s.done(w, r, "The action failed; the Kenfold server log has details.")
	}
}

// ---- memories ----

type memoryRow struct {
	Memory store.Memory
	Score  float64
	Refs   []store.CodeRef
}

type memoriesData struct {
	Rows     []memoryRow
	Query    string
	Scope    string
	Type     string
	Status   string
	Stale    bool
	Page     int
	More     bool
	Searched bool
	Note     string
	Scopes   []store.ScopeCount
	Types    []memory.Type
	Statuses []memory.Status
}

func (s *Server) memories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	d := memoriesData{
		Query: strings.TrimSpace(q.Get("q")), Scope: scopeParam(q.Get("scope")), Type: q.Get("type"), Status: q.Get("status"),
		Stale: q.Get("stale") == "1", Page: pageParam(r), Types: memory.Types,
		Statuses: []memory.Status{memory.StatusActive, memory.StatusProposed, memory.StatusSuperseded, memory.StatusDeleted},
	}
	if d.Status == "" {
		d.Status = string(memory.StatusActive)
	}
	var types []memory.Type
	if t, err := memory.ParseType(d.Type); err == nil && d.Type != "" {
		types = []memory.Type{t}
	} else {
		d.Type = ""
	}
	if st, err := s.d.Store.Stats(ctx); err == nil {
		d.Scopes = st.Scopes
	}
	switch {
	case d.Query != "" && d.Status == string(memory.StatusActive) && !d.Stale:
		d.Searched = true
		res, err := s.d.Retriever.Search(ctx, retrieve.Query{Text: truncateRunes(d.Query, 2000), Scope: d.Scope, AllScopes: d.Scope == "", Types: types, Limit: pageSize})
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, x := range res {
			d.Rows = append(d.Rows, memoryRow{Memory: x.Memory, Score: x.Score})
		}
	default:
		if d.Query != "" {
			d.Note = "Search covers active memories. Clear the search box to list other statuses or outdated memories."
		}
		p := store.ListParams{Types: types, Stale: d.Stale, Limit: pageSize + 1, Offset: (d.Page - 1) * pageSize}
		if st := memory.Status(d.Status); slices.Contains(d.Statuses, st) {
			p.Statuses = []memory.Status{st}
		} else if d.Status == "all" {
			p.Statuses = d.Statuses
		}
		if d.Scope != "" {
			p.Scopes = []string{d.Scope}
		}
		ms, err := s.d.Store.List(ctx, p)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		d.More = len(ms) > pageSize
		for _, m := range ms[:min(len(ms), pageSize)] {
			d.Rows = append(d.Rows, memoryRow{Memory: m})
		}
	}
	ids := make([]string, len(d.Rows))
	for i, row := range d.Rows {
		ids[i] = row.Memory.ID
	}
	if refs, err := s.d.Store.Refs(ctx, ids); err == nil {
		for i := range d.Rows {
			d.Rows[i].Refs = refs[d.Rows[i].Memory.ID]
		}
	}
	s.render(w, r, http.StatusOK, "memories.html", view{Title: "Memories", Nav: "memories", Data: d})
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

type detailData struct {
	Memory  store.Memory
	Refs    []store.CodeRef
	History []store.Memory
	Links   []store.Link
	Similar []store.Memory
	Attrs   map[string]string
}

func (s *Server) memoryDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := s.d.Store.Get(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidID) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := detailData{Memory: m, Attrs: map[string]string{}}
	for k, v := range m.Attrs {
		if str, ok := v.(string); ok {
			d.Attrs[k] = str
		} else {
			b, _ := json.Marshal(v)
			d.Attrs[k] = string(b)
		}
	}
	if refs, err := s.d.Store.Refs(ctx, []string{m.ID}); err == nil {
		d.Refs = refs[m.ID]
	}
	if h, err := s.d.Store.History(ctx, m.ID); err == nil && len(h) > 1 {
		d.History = h
	}
	d.Links, _ = s.d.Store.Links(ctx, m.ID)
	if m.Status == memory.StatusProposed {
		d.Similar = s.activeSimilar(ctx, m)
	}
	s.render(w, r, http.StatusOK, "memory.html", view{Title: "Memory " + m.ID[:8], Nav: "memories", Data: d})
}

func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if reason == "" {
		reason = "forgotten in the dashboard"
	}
	if utf8.RuneCountInString(reason) > 500 {
		reason = string([]rune(reason)[:500])
	}
	_, err := s.d.Store.SoftDelete(r.Context(), r.PathValue("id"), reason, Agent)
	switch {
	case err == nil:
		s.done(w, r, "Forgotten: the memory is no longer served; it is kept for audit.")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrInvalidID):
		s.done(w, r, "That memory no longer exists.")
	default:
		s.log.ErrorContext(r.Context(), "dashboard forget failed", "err", err)
		s.done(w, r, "The action failed; the Kenfold server log has details.")
	}
}

// ---- clients ----

type clientsData struct {
	Keys   []apikey.Key
	Grants []oauth.Grant
}

func (s *Server) clients(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var d clientsData
	var err error
	if d.Keys, err = s.d.Keys.List(ctx, false); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.Grants, err = s.d.Owner.Grants(ctx, false); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "clients.html", view{Title: "Clients", Nav: "clients", Data: d})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !store.ValidID(id) {
		s.done(w, r, "Unknown key.")
		return
	}
	k, err := s.d.Keys.Revoke(r.Context(), id)
	if err != nil {
		s.log.WarnContext(r.Context(), "dashboard: revoke key failed", "err", err)
		s.done(w, r, "The key could not be revoked: "+err.Error())
		return
	}
	s.log.InfoContext(r.Context(), "dashboard: API key revoked", "agent", k.Agent, "prefix", k.Prefix)
	s.done(w, r, "Revoked the key of "+k.Agent+" ("+k.Prefix+"…).")
}

func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !store.ValidID(id) {
		s.done(w, r, "Unknown client.")
		return
	}
	g, err := s.d.Owner.RevokeGrant(r.Context(), id)
	if err != nil {
		s.log.WarnContext(r.Context(), "dashboard: revoke grant failed", "err", err)
		s.done(w, r, "The client could not be revoked: "+err.Error())
		return
	}
	s.log.InfoContext(r.Context(), "dashboard: OAuth client revoked", "client_id", g.ClientID, "agent", g.Agent)
	s.done(w, r, "Revoked "+g.Agent+"; its tokens stopped working.")
}

// ---- consolidation ----

func (s *Server) consolidation(w http.ResponseWriter, r *http.Request) {
	if s.d.Consolidation == nil {
		s.render(w, r, http.StatusOK, "consolidation.html", view{Title: "Consolidation", Nav: "consolidation"})
		return
	}
	ps, err := s.d.Consolidation.Pending(r.Context(), 50)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "consolidation.html", view{Title: "Consolidation", Nav: "consolidation", Data: ps})
}

func (s *Server) applyProposal(w http.ResponseWriter, r *http.Request) {
	if s.d.Consolidation == nil {
		s.done(w, r, "Consolidation is not enabled.")
		return
	}
	msg, err := s.d.Consolidation.Apply(r.Context(), r.PathValue("id"), Agent)
	if err != nil {
		s.log.WarnContext(r.Context(), "dashboard: apply proposal failed", "err", err)
		s.done(w, r, "Not applied: "+err.Error())
		return
	}
	s.done(w, r, msg)
}

func (s *Server) rejectProposal(w http.ResponseWriter, r *http.Request) {
	if s.d.Consolidation == nil {
		s.done(w, r, "Consolidation is not enabled.")
		return
	}
	if err := s.d.Consolidation.Reject(r.Context(), r.PathValue("id"), Agent); err != nil {
		s.log.WarnContext(r.Context(), "dashboard: reject proposal failed", "err", err)
		s.done(w, r, "Not rejected: "+err.Error())
		return
	}
	s.done(w, r, "Rejected: these memories stay as they are, and this proposal will not be made again.")
}
