package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/migrations"
)

// fakeReadinessDB records how the probe uses the pool.
type fakeReadinessDB struct {
	pings   int
	queries int
	err     error
}

func (f *fakeReadinessDB) Ping(context.Context) error {
	f.pings++
	return f.err
}

func (f *fakeReadinessDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	f.queries++
	return fakeVersionRow{version: 0, err: f.err}
}

type fakeVersionRow struct {
	version int64
	err     error
}

func (r fakeVersionRow) Scan(dst ...any) error {
	if r.err != nil {
		return r.err
	}
	v := r.version
	*dst[0].(**int64) = &v
	return nil
}

// TestSchemaReadinessUsesThePool checks that the probe runs its schema check
// on the shared pool instead of opening a connection of its own. The previous
// implementation called migrations.CheckCurrent with the database URL on every
// unauthenticated request, which opened a new pool and re-read the embedded
// migrations each time.
func TestSchemaReadinessUsesThePool(t *testing.T) {
	db := &fakeReadinessDB{}
	if err := newSchemaReadiness(db).Ping(context.Background()); err == nil {
		t.Fatal("version 0 reported ready")
	} else if !errors.Is(err, migrations.ErrPending) {
		t.Errorf("err = %v, want ErrPending", err)
	}
	if db.pings != 1 || db.queries != 1 {
		t.Errorf("probe used the pool %d times for ping and %d for the schema check; want 1 each", db.pings, db.queries)
	}
}

// TestReadyzDoesNotOpenItsOwnConnectionIntegration saturates the pool's only
// connection and checks that /readyz fails. Opening a separate connection (the
// old behavior) would succeed here and hide pool exhaustion from the probe.
func TestReadyzDoesNotOpenItsOwnConnectionIntegration(t *testing.T) {
	url := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, url); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last-registered-first: release the connection before the
	// pool is closed, or Close would wait for it forever.
	t.Cleanup(pool.Close)

	// Hold the only connection for longer than the probe's timeout.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	holdCtx, release := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var n int
		_ = conn.QueryRow(holdCtx, `SELECT 1 FROM pg_sleep(10)`).Scan(&n)
	}()
	t.Cleanup(func() {
		release()
		<-done
		conn.Release()
	})

	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := newSchemaReadiness(pool).Ping(probeCtx); err == nil {
		t.Fatal("readiness succeeded while the pool had no free connection")
	}
}
