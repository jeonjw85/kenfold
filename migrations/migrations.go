// Package migrations embeds Kenfold's SQL migrations and applies them with goose.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed *.sql
var FS embed.FS

// Provider opens a goose provider over the embedded migrations. A Postgres
// advisory lock serializes concurrent runs (several replicas or clients
// starting at once). The caller must Close the provider, which also closes
// the DB handle.
func Provider(databaseURL string) (*goose.Provider, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migration lock: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, FS, goose.WithSessionLocker(locker))
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("goose provider: %w", err)
	}
	return p, nil
}

// Up applies all pending migrations and returns the resulting schema version.
func Up(ctx context.Context, databaseURL string) (int64, error) {
	p, err := Provider(databaseURL)
	if err != nil {
		return 0, err
	}
	defer p.Close()
	if _, err := p.Up(ctx); err != nil {
		return 0, fmt.Errorf("migrate up: %w", err)
	}
	return p.GetDBVersion(ctx)
}

// ErrPending means the database schema is older than this binary expects.
var ErrPending = errors.New("database schema is out of date; run `kenfold migrate` (or set KENFOLD_AUTO_MIGRATE=true)")

// QueryRower is the subset of pgxpool.Pool and pgx.Tx that CheckCurrentWith
// needs.
type QueryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TargetVersion returns the newest migration version embedded in this binary.
// It reads the file names only, so it needs no database.
func TargetVersion() (int64, error) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		return 0, fmt.Errorf("read embedded migrations: %w", err)
	}
	var target int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			continue
		}
		target = max(target, v)
	}
	if target == 0 {
		return 0, errors.New("no embedded migrations")
	}
	return target, nil
}

// CheckCurrentWith is CheckCurrent on an existing connection, so health probes
// reuse the server's pool instead of opening one per request.
func CheckCurrentWith(ctx context.Context, q QueryRower) error {
	target, err := TargetVersion()
	if err != nil {
		return err
	}
	var current *int64
	err = q.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version`).Scan(&current)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
		return fmt.Errorf("%w (database has no schema)", ErrPending)
	}
	if err != nil {
		return fmt.Errorf("check schema version: %w", err)
	}
	if current == nil {
		return fmt.Errorf("%w (database has no schema)", ErrPending)
	}
	if *current < target {
		return fmt.Errorf("%w (database at version %d, binary expects %d)", ErrPending, *current, target)
	}
	return nil
}

// CheckCurrent returns ErrPending if migrations are pending, or the connection
// error if the database cannot be reached.
func CheckCurrent(ctx context.Context, databaseURL string) error {
	p, err := Provider(databaseURL)
	if err != nil {
		return err
	}
	defer p.Close()
	current, target, err := p.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("check schema version: %w", err)
	}
	if current < target {
		return fmt.Errorf("%w (database at version %d, binary expects %d)", ErrPending, current, target)
	}
	return nil
}
