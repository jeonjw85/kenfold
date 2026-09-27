package apikey

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/kenfold/kenfold/migrations"
)

// TestAPIKeyIntegration runs against a throwaway PostgreSQL database and
// TRUNCATES api_key:
//
//	KENFOLD_TEST_DATABASE_URL=postgres://... go test -run Integration ./internal/apikey/
func TestAPIKeyIntegration(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `TRUNCATE api_key`); err != nil {
		t.Fatal(err)
	}
	s := NewStore(pool)

	for _, bad := range []string{"", "Claude Code", "-x", "a/b"} {
		if _, _, err := s.Create(ctx, bad); err == nil {
			t.Errorf("Create(%q) succeeded", bad)
		}
	}

	plain, k, err := s.Create(ctx, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !WellFormed(plain) || k.Agent != "claude-code" || k.Prefix != plain[:displayLen] || k.RevokedAt != nil || k.LastUsedAt != nil {
		t.Fatalf("created %q -> %+v", plain, k)
	}
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT key_hash FROM api_key WHERE id = $1`, k.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(stored) == plain || len(stored) != 32 {
		t.Fatal("plaintext key stored or wrong hash length")
	}

	got, err := s.Verify(ctx, plain)
	if err != nil || got.ID != k.ID || got.Agent != "claude-code" {
		t.Fatalf("verify = %+v, %v", got, err)
	}
	var lastUsed bool
	if err := pool.QueryRow(ctx, `SELECT last_used_at IS NOT NULL FROM api_key WHERE id = $1`, k.ID).Scan(&lastUsed); err != nil || !lastUsed {
		t.Errorf("last_used_at not recorded: %v", err)
	}
	other, _ := Generate()
	for _, bad := range []string{"", "garbage", other} {
		if _, err := s.Verify(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Verify(%q) err = %v, want ErrNotFound", bad, err)
		}
	}

	// TokenVerifier maps keys to agents and unknown keys to ErrInvalidToken.
	v := s.TokenVerifier(nil)
	ti, err := v(ctx, plain, nil)
	if a, ok := AgentFrom(ti); err != nil || !ok || a != "claude-code" || ti.UserID != k.ID {
		t.Errorf("TokenVerifier = %+v, %v", ti, err)
	}
	if _, err := v(ctx, other, nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("unknown key err = %v, want ErrInvalidToken", err)
	}

	_, k2, err := s.Create(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := s.List(ctx, false)
	if err != nil || len(keys) != 2 || keys[0].ID != k2.ID {
		t.Fatalf("list = %+v, %v", keys, err)
	}

	// Revoke by prefix; revoked keys stop verifying and are hidden by default.
	r, err := s.Revoke(ctx, k.Prefix)
	if err != nil || r.RevokedAt == nil {
		t.Fatalf("revoke = %+v, %v", r, err)
	}
	if _, err := s.Verify(ctx, plain); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked key still verifies: %v", err)
	}
	if again, err := s.Revoke(ctx, k.ID); err != nil || !again.RevokedAt.Equal(*r.RevokedAt) {
		t.Errorf("second revoke = %+v, %v; want idempotent", again, err)
	}
	if keys, _ := s.List(ctx, false); len(keys) != 1 {
		t.Errorf("active list has %d keys, want 1", len(keys))
	}
	if keys, _ := s.List(ctx, true); len(keys) != 2 {
		t.Errorf("full list has %d keys, want 2", len(keys))
	}
	if _, err := s.Revoke(ctx, "kf_nothere"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke unknown err = %v", err)
	}

	// Two keys sharing a displayed prefix cannot be revoked by prefix.
	if _, err := pool.Exec(ctx, `INSERT INTO api_key (agent, prefix, key_hash) VALUES ('x', 'kf_dupdupd', decode(repeat('aa', 32), 'hex')), ('y', 'kf_dupdupd', decode(repeat('bb', 32), 'hex'))`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revoke(ctx, "kf_dupdupd"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("ambiguous revoke err = %v", err)
	}
}
