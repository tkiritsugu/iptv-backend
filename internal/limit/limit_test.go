package limit

import (
	"testing"
	"time"

	"iptv/internal/testutil"
)

func newTestLimiter(burst int, window time.Duration) (*Limiter, *testutil.Clock) {
	clock := testutil.NewClock()
	limiter := New(burst, window)
	limiter.WithClock(clock.Now)
	return limiter, clock
}

// Only failures cost anything: a client with correct credentials must never run
// out of its own budget, and a client that is already blocked must not extend
// the wait by retrying.
func TestSuccessesDoNotConsumeBudget(t *testing.T) {
	limiter, _ := newTestLimiter(3, time.Minute)
	for range 100 {
		if !limiter.Allow("1.2.3.4") {
			t.Fatal("a fresh key was blocked without any failures")
		}
	}
}

func TestFailuresBlockAndRefill(t *testing.T) {
	limiter, clock := newTestLimiter(3, time.Minute)

	for range 3 {
		if !limiter.Allow("1.2.3.4") {
			t.Fatal("blocked before the allowance was used")
		}
		limiter.Failure("1.2.3.4")
	}
	if limiter.Allow("1.2.3.4") {
		t.Fatal("blocked after only the allowance worth of failures")
	}
	if got := limiter.RetryAfter("1.2.3.4"); got <= 0 {
		t.Errorf("RetryAfter = %v, want a positive wait", got)
	}

	// Another client is unaffected.
	if !limiter.Allow("5.6.7.8") {
		t.Error("the limit is not per client")
	}

	// A full window restores the whole allowance.
	clock.Advance(time.Minute)
	for range 3 {
		if !limiter.Allow("1.2.3.4") {
			t.Fatalf("refill failed at attempt %d", 3)
		}
		limiter.Failure("1.2.3.4")
	}
	if limiter.Allow("1.2.3.4") {
		t.Error("the allowance is not bounded by the burst")
	}
}

func TestDisabledWhenBurstIsZero(t *testing.T) {
	limiter, _ := newTestLimiter(0, time.Minute)
	for range 10 {
		limiter.Failure("1.2.3.4")
		if !limiter.Allow("1.2.3.4") {
			t.Fatal("a zero burst must disable the limiter")
		}
	}
}

func TestIdleKeysArePruned(t *testing.T) {
	limiter, clock := newTestLimiter(2, time.Minute)
	for i := range 50 {
		limiter.Failure("client-" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	before := len(limiter.buckets)
	clock.Advance(3 * time.Minute)
	limiter.Allow("newcomer")
	if len(limiter.buckets) >= before {
		t.Errorf("buckets grew from %d to %d instead of being pruned", before, len(limiter.buckets))
	}
}
