// Package testutil holds small helpers shared by the tests of several packages.
// It is used only from tests.
package testutil

import (
	"sync"
	"time"
)

// Clock is a manually advanced time source. Anything that depends on time —
// subscription expiry, cache TTLs — is testable through it without sleeping.
type Clock struct {
	mu sync.RWMutex
	t  time.Time
}

// NewClock returns a clock starting at a fixed, realistic moment.
func NewClock() *Clock {
	return &Clock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

// Now implements the func() time.Time contract.
func (c *Clock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.t
}

// Advance moves time forward, which is how "an hour later" is expressed in a
// test.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
