package oauth

import (
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := newRateLimiter(3, 1) // burst 3, one token per second
	l.now = func() time.Time { return now }

	for i := range 3 {
		if !l.allow() {
			t.Fatalf("request %d within the burst was refused", i+1)
		}
	}
	if l.allow() {
		t.Fatal("a request beyond the burst was allowed")
	}
	now = now.Add(900 * time.Millisecond)
	if l.allow() {
		t.Error("a request before a full token refilled was allowed")
	}
	now = now.Add(100 * time.Millisecond)
	if !l.allow() {
		t.Error("a request after one token refilled was refused")
	}
	// Tokens do not accumulate past the burst.
	now = now.Add(time.Hour)
	for i := range 3 {
		if !l.allow() {
			t.Fatalf("refill %d after a long idle period was refused", i+1)
		}
	}
	if l.allow() {
		t.Error("the bucket refilled past its burst")
	}
}
