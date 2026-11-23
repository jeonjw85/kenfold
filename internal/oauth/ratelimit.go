package oauth

import (
	"sync"
	"time"
)

// Unauthenticated OAuth endpoints are public by design, so each one that
// creates rows or makes an outbound request is bounded: a token bucket caps
// the request rate and the store caps how many clients may exist.
const (
	// Dynamic registration: a burst for a few clients starting at once, then
	// a slow trickle. The stored-client cap is the real bound.
	registerBurst     = 20
	registerPerSecond = 1
	// Authorization: consent pages are opened by one owner, but each first
	// sight of a metadata document URL triggers an outbound fetch.
	authorizeBurst     = 30
	authorizePerSecond = 5
	// Token and revocation requests are cheap but needed by every client, so
	// the budget is generous; it bounds a flood that would otherwise occupy
	// every database connection.
	tokenBurst      = 100
	tokenPerSecond  = 20
	revokeBurst     = 20
	revokePerSecond = 5
)

// rateLimiter is a token bucket with an injectable clock.
type rateLimiter struct {
	mu     sync.Mutex
	burst  float64
	refill float64 // tokens per second
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newRateLimiter(burst int, perSecond float64) *rateLimiter {
	return &rateLimiter{burst: float64(burst), refill: perSecond, tokens: float64(burst), now: time.Now}
}

// allow takes one token, refilling by elapsed time, and reports whether the
// request may proceed.
func (l *rateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.last.IsZero() {
		l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.refill)
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
