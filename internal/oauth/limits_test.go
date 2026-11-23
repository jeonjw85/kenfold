package oauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newLimitServer returns a server whose store is never touched: the rate
// limit is checked before any database work.
func newLimitServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(NewStore(nil), Config{PublicURL: "https://k.example", DCR: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRegisterRateLimited(t *testing.T) {
	s := newLimitServer(t)
	fixed := time.Unix(0, 0)
	s.registerLimit = newRateLimiter(2, 0)
	s.registerLimit.now = func() time.Time { return fixed }

	send := func() int {
		r := httptest.NewRequest(http.MethodPost, PathRegister, nil)
		w := httptest.NewRecorder()
		s.register(w, r)
		return w.Code
	}
	for i := range 2 {
		if code := send(); code == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate limited within the burst", i+1)
		}
	}
	w := httptest.NewRecorder()
	s.register(w, httptest.NewRequest(http.MethodPost, PathRegister, nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}

func TestAuthorizeRateLimited(t *testing.T) {
	s := newLimitServer(t)
	fixed := time.Unix(0, 0)
	s.authorizeLimit = newRateLimiter(2, 0)
	s.authorizeLimit.now = func() time.Time { return fixed }

	for i := range 2 {
		w := httptest.NewRecorder()
		s.authorize(w, httptest.NewRequest(http.MethodGet, PathAuthorize, nil))
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate limited within the burst", i+1)
		}
	}
	w := httptest.NewRecorder()
	s.authorize(w, httptest.NewRequest(http.MethodGet, PathAuthorize, nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429", w.Code)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errNetwork struct{}

func (errNetwork) Error() string { return "dial tcp: lookup client.example: errNetwork" }

func TestTokenRateLimited(t *testing.T) {
	s := newLimitServer(t)
	fixed := time.Unix(0, 0)
	s.tokenLimit = newRateLimiter(1, 0)
	s.tokenLimit.now = func() time.Time { return fixed }
	if !s.tokenLimit.allow() {
		t.Fatal("the first token was refused")
	}
	w := httptest.NewRecorder()
	s.token(w, httptest.NewRequest(http.MethodPost, PathToken, nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}

func TestRevokeRateLimited(t *testing.T) {
	s := newLimitServer(t)
	fixed := time.Unix(0, 0)
	s.revokeLimit = newRateLimiter(1, 0)
	s.revokeLimit.now = func() time.Time { return fixed }
	if !s.revokeLimit.allow() {
		t.Fatal("the first revocation was refused")
	}
	w := httptest.NewRecorder()
	s.revoke(w, httptest.NewRequest(http.MethodPost, PathRevoke, nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429", w.Code)
	}
}
