package dashboard

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/oauth"
	"github.com/kenfold/kenfold/migrations"
)

// Rotate the real database password immediately after verification commits,
// before login can read another stamp or create its session. No timing sleeps.
type passwordRotationTrace struct {
	once   sync.Once
	rotate func()
}

type commitTraceKey struct{}

func (tr *passwordRotationTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, commitTraceKey{}, d.SQL == "commit")
}

func (tr *passwordRotationTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	if commit, _ := ctx.Value(commitTraceKey{}).(bool); commit && d.Err == nil {
		tr.once.Do(tr.rotate)
	}
}

func TestLoginDuringPasswordRotationEndsOldPasswordSessionIntegration(t *testing.T) {
	dbURL := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner := oauth.NewStore(pool)
	const oldPassword = "the previous dashboard password"
	if err := owner.SetOwnerPassword(ctx, oldPassword); err != nil {
		t.Fatal(err)
	}
	tr := &passwordRotationTrace{rotate: func() {
		if err := owner.SetOwnerPassword(ctx, "the rotated dashboard password"); err != nil {
			t.Fatal(err)
		}
	}}
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = tr
	loginPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer loginPool.Close()
	s := &Server{d: Deps{Owner: oauth.NewStore(loginPool)}, log: slog.New(slog.DiscardHandler), now: time.Now, sessions: sessions{m: map[string]*session{}}}
	form := url.Values{"password": {oldPassword}}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.login(w, req)
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) != 1 {
		t.Fatalf("old-password verification did not finish: status=%d", w.Code)
	}
	if err := owner.CheckOwner(ctx, "the rotated dashboard password"); err != nil {
		t.Fatalf("rotation did not take effect: %v", err)
	}
	check := httptest.NewRequest(http.MethodGet, "http://localhost/dashboard/", nil)
	check.AddCookie(w.Result().Cookies()[0])
	w = httptest.NewRecorder()
	s.authed(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })(w, check)
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/dashboard/login?") {
		t.Errorf("session opened with old password survived rotation: status=%d", w.Code)
	}
}
