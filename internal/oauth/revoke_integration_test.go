package oauth

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/authz"
	"github.com/kenfold/kenfold/migrations"
)

// TestRevokeGrantPrefixIntegration checks that revoking by an id prefix cannot
// act as a LIKE pattern and revoke a grant the operator did not name, and that
// a unique prefix still works. It TRUNCATES oauth_client and oauth_grant.
func TestRevokeGrantPrefixIntegration(t *testing.T) {
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
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE oauth_client, oauth_grant, oauth_token CASCADE`); err != nil {
		t.Fatal(err)
	}
	s := NewStore(pool)

	// Two grants whose ids share the first 8 hex digits only by chance, so a
	// wildcard would have to widen the match rather than a genuine prefix.
	mk := func(clientID string) Grant {
		c := securityClient(t, s, kindCIMD, ClientMetadata{ClientID: clientID, ClientName: clientID, RedirectURIs: []string{clientID + "/cb"}})
		return securityGrant(t, s, c, []string{authz.ScopeRead})
	}
	a, b := mk("https://one.example/c"), mk("https://two.example/c")

	// '%' and '_' are not part of a UUID, so they are refused instead of
	// matching (and revoking) more than the operator named.
	for _, ref := range []string{"%%%%%%%%", "________", a.ID[:8] + "%", a.ID[:7] + "_"} {
		if _, err := s.RevokeGrant(ctx, ref); err == nil || !strings.Contains(err.Error(), "hex digits") {
			t.Errorf("RevokeGrant(%q) = %v, want a rejection", ref, err)
		}
	}
	var live int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM oauth_grant WHERE revoked_at IS NULL`).Scan(&live); err != nil || live != 2 {
		t.Fatalf("live grants after rejected refs = %d, %v; want 2", live, err)
	}

	// A unique prefix revokes exactly that grant, as the CLI integration test
	// expects (it passes the first 13 characters).
	g, err := s.RevokeGrant(ctx, a.ID[:13])
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != a.ID {
		t.Errorf("revoked %s, want %s", g.ID, a.ID)
	}
	grants, err := s.Grants(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var revoked, active []string
	for _, g := range grants {
		if g.RevokedAt != nil {
			revoked = append(revoked, g.ID)
		} else {
			active = append(active, g.ID)
		}
	}
	if !slices.Equal(revoked, []string{a.ID}) || !slices.Equal(active, []string{b.ID}) {
		t.Errorf("revoked=%v active=%v; want only %s revoked", revoked, active, a.ID)
	}

	// An ambiguous prefix still asks for more of the id.
	if _, err := s.RevokeGrant(ctx, "01a0e2f4"); err != nil && !errors.Is(err, errNotFound) {
		if !strings.Contains(err.Error(), "matches") {
			t.Errorf("ambiguous ref = %v", err)
		}
	}
	if _, err := s.RevokeGrant(ctx, b.ID[:8]); err != nil {
		t.Errorf("unique 8-character prefix: %v", err)
	}
}
