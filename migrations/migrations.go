// Package migrations embeds Kenfold's SQL migrations and applies them with goose.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

//go:embed *.sql
var FS embed.FS

// Provider opens a goose provider over the embedded migrations.
// The caller must Close the returned provider, which also closes the DB handle.
func Provider(databaseURL string) (*goose.Provider, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, FS)
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
