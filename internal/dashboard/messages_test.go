package dashboard

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/apikey"
	"github.com/kenfold/kenfold/internal/oauth"
)

// failingConsolidation returns err from both decision methods.
type failingConsolidation struct{ err error }

func (f failingConsolidation) Pending(context.Context, int) ([]Proposal, error) { return nil, nil }
func (f failingConsolidation) PendingCount(context.Context) (int, error)        { return 0, nil }
func (f failingConsolidation) Apply(context.Context, string, string) (string, error) {
	return "", f.err
}
func (f failingConsolidation) Reject(context.Context, string, string) error { return f.err }

// TestFailedActionHidesInternalErrors checks that a failed dashboard action
// reports a generic message. Store and driver errors carry SQL text, table and
// constraint names, and connection details, which must go to the log only.
func TestFailedActionHidesInternalErrors(t *testing.T) {
	// Closing the pool makes every query fail with an internal driver error
	// ("closed pool"), without needing a database.
	pool, err := pgxpool.New(context.Background(), "postgres://kenfold:kenfold@127.0.0.1:1/kenfold?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()

	cases := []struct {
		name string
		s    *Server
		call func(*Server, http.ResponseWriter, *http.Request)
		path string
	}{
		{
			name: "revoke key",
			s:    &Server{d: Deps{Keys: apikey.NewStore(pool)}, log: slog.New(slog.DiscardHandler)},
			call: (*Server).revokeKey,
			path: "/dashboard/clients/01a0e2f4-5446-7fe9-bb7a-89d49ad44bfa/revoke",
		},
		{
			name: "revoke grant",
			s:    &Server{d: Deps{Owner: oauth.NewStore(pool)}, log: slog.New(slog.DiscardHandler)},
			call: (*Server).revokeGrant,
			path: "/dashboard/clients/01a0e2f4-5446-7fe9-bb7a-89d49ad44bfa/revoke",
		},
		{
			name: "apply proposal",
			s:    &Server{d: Deps{Consolidation: failingConsolidation{err: errInternal}}, log: slog.New(slog.DiscardHandler)},
			call: (*Server).applyProposal,
			path: "/dashboard/consolidation/01a0e2f4-5446-7fe9-bb7a-89d49ad44bfa/apply",
		},
		{
			name: "reject proposal",
			s:    &Server{d: Deps{Consolidation: failingConsolidation{err: errInternal}}, log: slog.New(slog.DiscardHandler)},
			call: (*Server).rejectProposal,
			path: "/dashboard/consolidation/01a0e2f4-5446-7fe9-bb7a-89d49ad44bfa/reject",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.s.sessions = sessions{m: map[string]*session{}}
			sess := c.s.sessions.create("stamp", time.Now())
			r := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(url.Values{"back": {Prefix}}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			r.SetPathValue("id", "01a0e2f4-5446-7fe9-bb7a-89d49ad44bfa")
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess))
			w := httptest.NewRecorder()
			c.call(c.s, w, r)

			msg := c.s.sessions.takeFlash(sess)
			if msg == "" {
				t.Fatal("no message")
			}
			for _, leak := range []string{"closed pool", "pgx", "SQL", "syntax", "constraint", "pq:"} {
				if strings.Contains(msg, leak) {
					t.Errorf("message leaks internals (%q): %q", leak, msg)
				}
			}
			if !strings.Contains(msg, "log has details") {
				t.Errorf("message = %q; want a pointer to the server log", msg)
			}
		})
	}
}

// errInternal stands in for any unexpected store error.
var errInternal = errTest("store: SELECT failed: relation \"memory\" does not exist")

type errTest string

func (e errTest) Error() string { return string(e) }
