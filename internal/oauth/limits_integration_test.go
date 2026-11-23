package oauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/authz"
	"github.com/kenfold/kenfold/migrations"
)

// TestCIMDFetchConcurrencyLimit checks that outbound fetches made for
// unauthenticated callers are bounded, and that a slot is released again.
func TestCIMDFetchConcurrencyLimit(t *testing.T) {
	f := NewCIMDFetcher(true)
	for range maxConcurrentFetches {
		f.slots <- struct{}{}
	}
	if _, err := f.Fetch(context.Background(), "https://client.example/meta.json"); !errors.Is(err, errFetchBusy) {
		t.Fatalf("fetch with every slot busy: %v, want errFetchBusy", err)
	}
	for range maxConcurrentFetches {
		<-f.slots
	}

	const doc = `{"client_id":"https://client.example/meta.json","redirect_uris":["https://client.example/cb"]}`
	f.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(doc)), Header: make(http.Header)}, nil
	})
	m, err := f.Fetch(context.Background(), "https://client.example/meta.json")
	if err != nil || m.ClientID != "https://client.example/meta.json" {
		t.Fatalf("fetch after releasing slots: %+v, %v", m, err)
	}
}

// TestJWKSCacheBounded checks that the key-set cache evicts instead of growing.
func TestJWKSCacheBounded(t *testing.T) {
	var c jwksCache
	now := time.Unix(0, 0)
	for i := range maxJwksEntries {
		c.put(fmt.Sprintf("https://client.example/%d", i), nil, now.Add(time.Duration(i)*time.Second))
	}
	c.put("https://client.example/newest", nil, now.Add(time.Hour))
	if len(c.entries) != maxJwksEntries {
		t.Fatalf("cache holds %d entries, want %d", len(c.entries), maxJwksEntries)
	}
	if _, ok := c.entries["https://client.example/0"]; ok {
		t.Error("the oldest entry was kept")
	}
	if _, ok := c.entries["https://client.example/newest"]; !ok {
		t.Error("the newest entry is missing")
	}
}

func limitsStore(t *testing.T) *Store {
	t.Helper()
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
	return NewStore(pool)
}

func testClient(id string) *ClientMetadata {
	return &ClientMetadata{ClientID: id, ClientName: id, RedirectURIs: []string{"https://client.example/cb"}}
}

// TestClientCapIntegration checks that the client table cannot grow past the
// cap, that abandoned clients are evicted to make room, and that a client
// with a grant is never dropped.
func TestClientCapIntegration(t *testing.T) {
	s := limitsStore(t)
	ctx := context.Background()
	s.maxClients = 3

	ids := []string{"https://a.example/c", "https://b.example/c", "https://c.example/c"}
	for _, id := range ids {
		if _, err := s.putClient(ctx, kindDCR, testClient(id), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.putClient(ctx, kindDCR, testClient("https://d.example/c"), false); !errors.Is(err, errTooManyClients) {
		t.Fatalf("4th client: %v, want errTooManyClients", err)
	}

	// The oldest client has been waiting too long without an approval; a new
	// registration may replace it.
	if _, err := s.pool.Exec(ctx, `UPDATE oauth_client SET created_at = now() - interval '2 hours' WHERE client_id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.putClient(ctx, kindDCR, testClient("https://d.example/c"), false); err != nil {
		t.Fatalf("registration after eviction: %v", err)
	}
	if _, err := s.getClient(ctx, ids[0]); !errors.Is(err, errNotFound) {
		t.Errorf("abandoned client was kept: %v", err)
	}

	// A client the owner approved stays, even when old.
	if _, err := s.pool.Exec(ctx, `UPDATE oauth_client SET created_at = now() - interval '2 hours' WHERE client_id = $1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createGrant(ctx, ids[1], "agent-b", []string{authz.ScopeRead}, "https://k.example/mcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE oauth_client SET created_at = now() - interval '2 hours' WHERE client_id = $1`, ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.putClient(ctx, kindDCR, testClient("https://e.example/c"), false); err != nil {
		t.Fatalf("registration with only an approved client left: %v", err)
	}
	if _, err := s.getClient(ctx, ids[1]); err != nil {
		t.Errorf("approved client was evicted: %v", err)
	}
}

// TestPruneAbandonedClientsIntegration checks the retention: a client is
// pruned after an hour without a grant, and kept while onboarding may still
// be in progress or once approved.
func TestPruneAbandonedClientsIntegration(t *testing.T) {
	s := limitsStore(t)
	ctx := context.Background()

	old := "https://old.example/c"
	young := "https://young.example/c"
	approved := "https://approved.example/c"
	for _, id := range []string{old, young, approved} {
		if _, err := s.putClient(ctx, kindCIMD, testClient(id), true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.createGrant(ctx, approved, "agent", []string{authz.ScopeRead}, "https://k.example/mcp"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{old, approved} {
		if _, err := s.pool.Exec(ctx, `UPDATE oauth_client SET created_at = now() - interval '2 hours' WHERE client_id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE oauth_client SET created_at = now() - interval '30 minutes' WHERE client_id = $1`, young); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.getClient(ctx, old); !errors.Is(err, errNotFound) {
		t.Errorf("abandoned client survived prune: %v", err)
	}
	if _, err := s.getClient(ctx, young); err != nil {
		t.Errorf("a client still onboarding was pruned: %v", err)
	}
	if _, err := s.getClient(ctx, approved); err != nil {
		t.Errorf("an approved client was pruned: %v", err)
	}
}

// TestAuthorizeDoesNotLeakFetchErrors checks that an unauthenticated caller
// does not see fetch details on the error page; they go to the server log.
func TestAuthorizeDoesNotLeakFetchErrors(t *testing.T) {
	s := limitsStore(t)
	srv, err := New(s, Config{PublicURL: "https://k.example"})
	if err != nil {
		t.Fatal(err)
	}
	srv.SetFetcherForTest(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: lookup client.example: server misbehaving")
	}))
	r := httptest.NewRequest(http.MethodGet, PathAuthorize+"?client_id=https://client.example/meta.json", nil)
	w := httptest.NewRecorder()
	srv.authorize(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	for _, leak := range []string{"dial", "lookup", "server misbehaving", "fetch client metadata"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("error page leaks %q: %q", leak, w.Body.String())
		}
	}
}
