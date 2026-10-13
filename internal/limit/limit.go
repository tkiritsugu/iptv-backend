// Package limit provides a small in-process token bucket limiter used to slow
// down password guessing against the Basic Auth realm.
//
// It is deliberately per instance: the counter lives in memory, so a restart
// resets it and a fleet of instances multiplies the allowance by its size. That
// is acceptable for the rate this protects (an interactive login attempts are
// already bounded by the hashing cost) and avoids inventing distributed state
// the task does not need. The real fix at scale is a proxy level limit, which is
// noted in the README.
package limit

import (
	"sync"
	"time"
)

// Limiter allows burst attempts per key per window.
type Limiter struct {
	burst  float64
	refill float64 // tokens per second
	ttl    time.Duration

	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	last    time.Time
}

type bucket struct {
	tokens  float64
	updated time.Time
}

// New returns a limiter allowing burst failures per key per window. A burst of
// zero disables the limiter.
func New(burst int, window time.Duration) *Limiter {
	if burst <= 0 || window <= 0 {
		return &Limiter{burst: 0}
	}
	return &Limiter{
		burst:   float64(burst),
		refill:  float64(burst) / window.Seconds(),
		ttl:     2 * window,
		now:     time.Now,
		buckets: make(map[string]*bucket),
	}
}

// WithClock replaces the time source, for tests.
func (l *Limiter) WithClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// Allow reports whether the key may proceed. It does not consume anything: only
// a failure costs a token, so a client that keeps authenticating correctly can
// never use up its own budget, and a client that is already blocked does not
// extend the lockout by retrying.
func (l *Limiter) Allow(key string) bool {
	if l.burst == 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refillLocked(key, l.now()).tokens >= 1
}

// Failure charges one token to a key; this is what a rejected login costs.
func (l *Limiter) Failure(key string) {
	if l.burst == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refillLocked(key, l.now())
	b.tokens--
	if b.tokens < 0 {
		b.tokens = 0
	}
}

// refillLocked returns the bucket of a key after applying accrued refill.
func (l *Limiter) refillLocked(key string, now time.Time) *bucket {
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, updated: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.updated).Seconds() * l.refill
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.updated = now
	return b
}

// RetryAfter reports how long the key should wait before the next attempt.
func (l *Limiter) RetryAfter(key string) time.Duration {
	if l.burst == 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		return 0
	}
	missing := 1 - b.tokens
	if missing <= 0 {
		return 0
	}
	return time.Duration(missing/l.refill*float64(time.Second)) + time.Second
}

// purgeLocked drops keys that have been idle long enough to have refilled,
// keeping the map from growing with every client that ever failed.
func (l *Limiter) purgeLocked(now time.Time) {
	if now.Sub(l.last) < l.ttl {
		return
	}
	l.last = now
	for k, b := range l.buckets {
		if now.Sub(b.updated) > l.ttl {
			delete(l.buckets, k)
		}
	}
}
