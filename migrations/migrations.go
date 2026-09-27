// Package migrations embeds Kenfold's SQL migrations and applies them with goose.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

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
