// Package dashboard is Kenfold's web interface for the owner: review
// proposed memories (extracted memories, preferences, consolidation
// proposals), browse and search memory, clean up outdated memories, and
// manage the clients that can reach it.
//
// It is server-rendered HTML without scripts, mounted at /dashboard/ on the
// HTTP server. Access is limited in layers:
//
//   - Host: loopback host names only (compose publishes Kenfold on
//     127.0.0.1), unless remote access is enabled for the public URL's host;
//     requests for any other host get 404. In local mode, requests that
//     carry proxy forwarding headers (X-Forwarded-For and the like) are
//     refused too, so a tunnel that rewrites the Host header to localhost
//     does not expose the dashboard.
//   - Login with the owner password (Argon2id, with the lockout shared with
//     the OAuth consent page). Sessions live in memory, expire after 2 hours
//     idle (12 hours at most), and end when the owner password changes.
//   - Every state change is a POST that carries the session's CSRF token, is
//     subject to cross-origin protection, and uses a SameSite=Strict cookie.
//   - A content security policy that allows no scripts, framing, or foreign
//     form targets.
package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kenfold/kenfold/internal/apikey"
	"github.com/kenfold/kenfold/internal/oauth"
	"github.com/kenfold/kenfold/internal/retrieve"
	"github.com/kenfold/kenfold/internal/store"
)

// Prefix is where the dashboard is mounted.
const Prefix = "/dashboard/"

// Agent is recorded on changes made in the dashboard (rejections, forgets).
const Agent = "kenfold-dashboard"

const (
	cookieName     = "kenfold_dashboard"
	sessionIdle    = 2 * time.Hour
	sessionMax     = 12 * time.Hour
	maxSessions    = 64
	maxFormBytes   = 64 << 10
	pageSize       = 50
	reviewPageSize = 20
)

//go:embed templates/*.html static/*
var assets embed.FS

// Deps are the dashboard's dependencies.
type Deps struct {
	Store     *store.Store
	Keys      *apikey.Store
	Owner     *oauth.Store // owner password and OAuth grants
	Retriever *retrieve.Retriever
	// Consolidation, if set, lists and decides consolidation proposals.
	Consolidation Consolidation
	Logger        *slog.Logger
	Version       string
	// Remote also serves the dashboard on PublicHost (KENFOLD_DASHBOARD=remote).
	Remote     bool
	PublicHost string
	Now        func() time.Time
}

// Server serves the dashboard.
type Server struct {
	d        Deps
	log      *slog.Logger
	pages    map[string]*template.Template
	sessions sessions
	now      func() time.Time
	csrfOK   *http.CrossOriginProtection
}

// New returns the dashboard handler (paths include Prefix).
func New(d Deps) (http.Handler, error) {
	s := &Server{d: d, log: d.Logger, now: d.Now, csrfOK: http.NewCrossOriginProtection()}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.sessions.m = map[string]*session{}
	var err error
	if s.pages, err = parsePages(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET "+Prefix+"static/", http.StripPrefix(Prefix+"static/", staticHandler(http.FileServerFS(static))))
	mux.HandleFunc("GET "+Prefix+"login", s.loginPage)
	mux.HandleFunc("POST "+Prefix+"login", s.login)
	mux.HandleFunc("POST "+Prefix+"logout", s.authed(s.logout))
	mux.HandleFunc("GET "+Prefix+"{$}", s.authed(s.overview))
	mux.HandleFunc("GET "+Prefix+"review", s.authed(s.review))
	mux.HandleFunc("POST "+Prefix+"review/{id}/approve", s.authed(s.approve))
	mux.HandleFunc("POST "+Prefix+"review/{id}/replace", s.authed(s.approveReplacing))
	mux.HandleFunc("POST "+Prefix+"review/{id}/reject", s.authed(s.reject))
	mux.HandleFunc("GET "+Prefix+"memories", s.authed(s.memories))
	mux.HandleFunc("GET "+Prefix+"memories/{id}", s.authed(s.memoryDetail))
	mux.HandleFunc("POST "+Prefix+"memories/{id}/forget", s.authed(s.forget))
	mux.HandleFunc("GET "+Prefix+"clients", s.authed(s.clients))
	mux.HandleFunc("POST "+Prefix+"clients/keys/{id}/revoke", s.authed(s.revokeKey))
	mux.HandleFunc("POST "+Prefix+"clients/grants/{id}/revoke", s.authed(s.revokeGrant))
	mux.HandleFunc("GET "+Prefix+"consolidation", s.authed(s.consolidation))
	mux.HandleFunc("POST "+Prefix+"consolidation/{id}/apply", s.authed(s.applyProposal))
	mux.HandleFunc("POST "+Prefix+"consolidation/{id}/reject", s.authed(s.rejectProposal))
	mux.HandleFunc(Prefix, func(w http.ResponseWriter, r *http.Request) { s.notFound(w, r) })

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowed(r) {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if r.Method == http.MethodPost {
			if err := s.csrfOK.Check(r); err != nil {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		}
		mux.ServeHTTP(w, r)
	}), nil
}

func staticHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

// forwardingHeaders are added by reverse proxies and tunnels (Caddy, nginx,
// cloudflared, Tailscale Funnel); a browser on this machine sends none.
var forwardingHeaders = []string{"X-Forwarded-For", "X-Forwarded-Host", "Forwarded", "X-Real-Ip", "Cf-Connecting-Ip", "True-Client-Ip"}

// allowed reports whether the dashboard answers r.
func (s *Server) allowed(r *http.Request) bool {
	if s.d.Remote && s.d.PublicHost != "" && hostName(r.Host) == s.d.PublicHost {
		return true
	}
	for _, h := range forwardingHeaders {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return s.allowedHost(r.Host)
}

func hostName(host string) string {
	h := strings.ToLower(host)
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.Trim(h, "[]")
}

// allowedHost reports whether the dashboard answers for host.
func (s *Server) allowedHost(host string) bool {
	h := strings.ToLower(host)
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	return s.d.Remote && s.d.PublicHost != "" && h == s.d.PublicHost
}

// secureCookie reports whether the request came through the https public URL.
func (s *Server) secureCookie(r *http.Request) bool {
	h := strings.ToLower(r.Host)
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return r.TLS != nil || (s.d.PublicHost != "" && h == s.d.PublicHost)
}

// ---- sessions ----

type session struct {
	id, csrf, stamp string
	created, seen   time.Time
	flash           string
}

type sessions struct {
	mu sync.Mutex
	m  map[string]*session
}

func token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (ss *sessions) create(stamp string, now time.Time) *session {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for id, s := range ss.m {
		if now.Sub(s.seen) > sessionIdle || now.Sub(s.created) > sessionMax {
			delete(ss.m, id)
		}
	}
	for len(ss.m) >= maxSessions {
		var oldest *session
		for _, s := range ss.m {
			if oldest == nil || s.seen.Before(oldest.seen) {
				oldest = s
			}
		}
		delete(ss.m, oldest.id)
	}
	s := &session{id: token(), csrf: token(), stamp: stamp, created: now, seen: now}
	ss.m[s.id] = s
	return s
}

func (ss *sessions) get(id string, now time.Time) *session {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := ss.m[id]
	if s == nil {
		return nil
	}
	if now.Sub(s.seen) > sessionIdle || now.Sub(s.created) > sessionMax {
		delete(ss.m, id)
		return nil
	}
	s.seen = now
	return s
}

func (ss *sessions) drop(id string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.m, id)
}

func (ss *sessions) setFlash(s *session, msg string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s.flash = msg
}

func (ss *sessions) takeFlash(s *session) string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	f := s.flash
	s.flash = ""
	return f
}

type ctxKey struct{}

func sessionOf(ctx context.Context) *session { s, _ := ctx.Value(ctxKey{}).(*session); return s }

// authed requires a live session; POST requests also need its CSRF token.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var sess *session
		if c, err := r.Cookie(cookieName); err == nil {
			sess = s.sessions.get(c.Value, s.now())
		}
		if sess != nil {
			// Changing the owner password ends every session.
			stamp, err := s.d.Owner.OwnerStamp(r.Context())
			if err != nil || subtle.ConstantTimeCompare([]byte(stamp), []byte(sess.stamp)) != 1 {
				s.sessions.drop(sess.id)
				sess = nil
			}
		}
		if sess == nil {
			if r.Method != http.MethodGet {
				http.Error(w, "your session has ended; log in again", http.StatusForbidden)
				return
			}
			http.Redirect(w, r, Prefix+"login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(sess.csrf)) != 1 {
				http.Error(w, "the form has expired; reload the page and try again", http.StatusForbidden)
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	}
}

// ---- login ----

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil && s.sessions.get(c.Value, s.now()) != nil {
		http.Redirect(w, r, safeBack(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, "", http.StatusOK)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, msg string, status int) {
	set, _ := s.d.Owner.OwnerPasswordSet(r.Context())
	s.render(w, r, status, "login.html", view{Title: "Log in", Error: msg, Data: map[string]any{
		"Next": safeBack(r.FormValue("next")), "NoPassword": !set,
	}})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	stamp, err := s.d.Owner.CheckOwnerStamp(r.Context(), r.PostForm.Get("password"))
	switch {
	case errors.Is(err, oauth.ErrNoPassword):
		s.renderLogin(w, r, "No owner password is set. On the server, run: kenfold password", http.StatusForbidden)
		return
	case errors.Is(err, oauth.ErrOwnerLocked):
		s.log.WarnContext(r.Context(), "dashboard login locked after failed password attempts")
		s.renderLogin(w, r, "Too many wrong passwords. Try again in 15 minutes.", http.StatusTooManyRequests)
		return
	case errors.Is(err, oauth.ErrWrongPassword):
		s.log.WarnContext(r.Context(), "dashboard login: wrong owner password")
		s.renderLogin(w, r, "Wrong password.", http.StatusUnauthorized)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	sess := s.sessions.create(stamp, s.now())
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: sess.id, Path: Prefix, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: s.secureCookie(r), MaxAge: int(sessionMax.Seconds()),
	})
	s.log.InfoContext(r.Context(), "dashboard login")
	http.Redirect(w, r, safeBack(r.PostForm.Get("next")), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.sessions.drop(sessionOf(r.Context()).id)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: Prefix, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.secureCookie(r)})
	http.Redirect(w, r, Prefix+"login", http.StatusSeeOther)
}

// safeBack returns p if it is a dashboard path on this server, else the overview.
func safeBack(p string) string {
	if !strings.HasPrefix(p, Prefix) || strings.ContainsAny(p, "\\\r\n") || strings.Contains(p, "//") || strings.Contains(p, "/../") {
		return Prefix
	}
	if u, err := url.Parse(p); err != nil || u.IsAbs() || u.Host != "" {
		return Prefix
	}
	return p
}

// done redirects after an action, with a message for the next page.
func (s *Server) done(w http.ResponseWriter, r *http.Request, msg string) {
	s.sessions.setFlash(sessionOf(r.Context()), msg)
	http.Redirect(w, r, safeBack(r.PostForm.Get("back")), http.StatusSeeOther)
}
