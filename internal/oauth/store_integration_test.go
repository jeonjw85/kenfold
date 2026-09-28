package oauth

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/migrations"
)

// TestOwnerLockoutIntegration checks that failed password attempts are
// counted durably (they must survive the transaction) and lock the consent
// page. It TRUNCATES oauth_owner.
func TestOwnerLockoutIntegration(t *testing.T) {
	url := os.Getenv("KENFOLD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("KENFOLD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if _, err := migrations.Up(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE oauth_owner`); err != nil {
		t.Fatal(err)
	}
	s := NewStore(pool)
	now := time.Now()
	s.now = func() time.Time { return now }

	if err := s.CheckOwner(ctx, "anything at all"); !errors.Is(err, errNoPassword) {
		t.Fatalf("no password set: %v", err)
	}
	const pw = "a long owner password"
	if err := s.SetOwnerPassword(ctx, pw); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckOwner(ctx, pw); err != nil {
		t.Fatalf("right password: %v", err)
	}
	for i := 1; i < maxFailedLogins; i++ {
		if err := s.CheckOwner(ctx, "wrong password here"); !errors.Is(err, errInvalid) {
			t.Fatalf("attempt %d: %v", i, err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT failed_attempts FROM oauth_owner`).Scan(&n); err != nil || n != i {
			t.Fatalf("failed_attempts after %d failures = %d, %v", i, n, err)
		}
	}
	if err := s.CheckOwner(ctx, "wrong password here"); !errors.Is(err, errInvalid) {
		t.Fatalf("last failure: %v", err)
	}
	if err := s.CheckOwner(ctx, pw); !errors.Is(err, errLocked) {
		t.Fatalf("right password while locked: %v", err)
	}
	now = now.Add(lockoutPeriod + time.Second)
	if err := s.CheckOwner(ctx, pw); err != nil {
		t.Fatalf("after the lockout: %v", err)
	}
	// Setting the password again clears a lockout.
	for i := 0; i < maxFailedLogins; i++ {
		_ = s.CheckOwner(ctx, "wrong password here")
	}
	if err := s.SetOwnerPassword(ctx, pw+" 2"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckOwner(ctx, pw+" 2"); err != nil {
		t.Fatalf("after reset: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE oauth_owner`); err != nil {
		t.Fatal(err)
	}
}
