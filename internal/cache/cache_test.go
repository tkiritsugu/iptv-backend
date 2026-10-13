package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iptv/internal/testutil"
)

// fakeSource is a VersionSource under test control: the version only moves when
// a test says so, and every read is counted so the polling budget can be
// asserted.
type fakeSource struct {
	value atomic.Int64
	reads atomic.Int64
	fail  atomic.Bool
	err   error
}

func (f *fakeSource) Version(context.Context, string) (int64, error) {
	f.reads.Add(1)
	if f.fail.Load() {
		return 0, f.err
	}
	return f.value.Load(), nil
}

func (f *fakeSource) bump() int64 { return f.value.Add(1) }

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newTestCache builds a cache and its version reader on one shared clock.
func newTestCache(t *testing.T, ttl, stale time.Duration, maxEntries int) (*Cache, *Versions, *fakeSource, *testutil.Clock) {
	t.Helper()
	clock := testutil.NewClock()
	source := &fakeSource{err: errors.New("source unavailable")}
	source.value.Store(7)

	versions := NewVersions(source, time.Second, discardLogger())
	versions.WithClock(clock.Now)

	c := New(versions, "test", "catalog", ttl, stale, maxEntries, discardLogger(), WithClock(clock.Now))
	return c, versions, source, clock
}

func constant(value any, ttl time.Duration) Loader {
	return func(context.Context) (any, time.Duration, error) { return value, ttl, nil }
}

func TestValueIsBuiltOnceAndReusedWhileVersionHolds(t *testing.T) {
	c, _, _, _ := newTestCache(t, time.Minute, time.Minute, 64)

	var loads int
	loader := func(context.Context) (any, time.Duration, error) {
		loads++
		return "payload", 0, nil
	}

	for i := 0; i < 5; i++ {
		got, err := c.Get(context.Background(), "k", loader)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != "payload" {
			t.Fatalf("got %v, want payload", got)
		}
	}
	if loads != 1 {
		t.Errorf("loader ran %d times, want 1", loads)
	}
	if stats := c.Stats(); stats.Hits != 4 {
		t.Errorf("hits = %d, want 4", stats.Hits)
	}
}

func TestVersionChangeInvalidatesTheEntry(t *testing.T) {
	// The entry itself would stay valid for an hour: only the data version
	// moving causes a rebuild. Advancing the clock past the version interval is
	// how another instance's write becomes visible here.
	c, _, source, clock := newTestCache(t, time.Hour, time.Hour, 64)

	var seq int
	loader := func(context.Context) (any, time.Duration, error) {
		seq++
		return seq, 0, nil
	}

	// Read twice while the version holds (one load), then bump and let the
	// interval pass: the next read must rebuild.
	for step, want := range []int{1, 1, 2, 2} {
		if step == 2 {
			source.bump()
			clock.Advance(2 * time.Second)
		}
		got, err := c.Get(context.Background(), "k", loader)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != want {
			t.Fatalf("step %d: got %v, want %v", step, got, want)
		}
	}
	if seq != 2 {
		t.Errorf("loader ran %d times, want 2", seq)
	}
}

// TestInFlightSlotIsReleased is the regression test for a leaked single-flight
// entry: the first load registered a call and never removed it, so every later
// request for that key was answered with the first result for the lifetime of
// the process. A newly granted subscription kept returning "not allowed".
func TestInFlightSlotIsReleased(t *testing.T) {
	c, _, source, _ := newTestCache(t, time.Hour, time.Hour, 64)

	var loads int
	loader := func(context.Context) (any, time.Duration, error) {
		loads++
		return loads, 0, nil
	}

	if got, err := c.Get(context.Background(), "k", loader); err != nil || got != 1 {
		t.Fatalf("first Get = (%v, %v), want (1, nil)", got, err)
	}
	source.bump()
	c.InvalidateAll()

	if got, err := c.Get(context.Background(), "k", loader); err != nil || got != 2 {
		t.Fatalf("second Get = (%v, %v), want (2, nil)", got, err)
	}
	if stats := c.Stats(); stats.Collisions != 0 {
		t.Errorf("sequential requests collided %d times, want 0", stats.Collisions)
	}
}

func TestConcurrentMissesCollapseIntoOneLoad(t *testing.T) {
	c, _, _, _ := newTestCache(t, time.Hour, time.Hour, 64)

	const goroutines = 64
	var (
		loads   atomic.Int64
		release = make(chan struct{})
		arrived sync.WaitGroup
	)
	arrived.Add(goroutines)

	loader := func(context.Context) (any, time.Duration, error) {
		loads.Add(1)
		<-release
		return "shared", 0, nil
	}

	var (
		wg  sync.WaitGroup
		got = make([]any, goroutines)
	)
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			arrived.Done()
			value, err := c.Get(context.Background(), "hot", loader)
			if err != nil {
				t.Errorf("Get: %v", err)
			}
			got[i] = value
		}(i)
	}
	arrived.Wait()
	// Give the goroutines a moment to pile up on the same key before releasing
	// the loader; the point of the test is that they all wait for one load.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := loads.Load(); n != 1 {
		t.Errorf("loader ran %d times, want 1", n)
	}
	for i, value := range got {
		if value != "shared" {
			t.Errorf("goroutine %d got %v, want shared", i, value)
		}
	}
	if stats := c.Stats(); stats.Collisions == 0 {
		t.Error("expected collisions to be counted for waiting requests")
	}
}

func TestEntryExpiresOnTTL(t *testing.T) {
	c, _, _, clock := newTestCache(t, 10*time.Second, 10*time.Second, 64)

	var loads int
	loader := func(context.Context) (any, time.Duration, error) {
		loads++
		return loads, 0, nil
	}

	if got, _ := c.Get(context.Background(), "k", loader); got != 1 {
		t.Fatalf("got %v, want 1", got)
	}
	clock.Advance(5 * time.Second)
	if got, _ := c.Get(context.Background(), "k", loader); got != 1 {
		t.Fatalf("within the TTL got %v, want the cached 1", got)
	}
	clock.Advance(6 * time.Second)
	if got, _ := c.Get(context.Background(), "k", loader); got != 2 {
		t.Fatalf("after the TTL got %v, want a rebuilt 2", got)
	}
}

// A loader may shorten the lifetime: data that becomes false on its own (a
// subscription reaching its expiry) must not be cached for the default TTL.
func TestLoaderTTLOverridesDefault(t *testing.T) {
	c, _, _, clock := newTestCache(t, time.Hour, time.Hour, 64)

	var loads int
	loader := func(context.Context) (any, time.Duration, error) {
		loads++
		return loads, 2 * time.Second, nil
	}

	c.Get(context.Background(), "k", loader)
	clock.Advance(3 * time.Second)
	if got, _ := c.Get(context.Background(), "k", loader); got != 2 {
		t.Fatalf("got %v, want 2: the short ttl must be honoured", got)
	}
}

func TestStaleValueServedWhenReloadFails(t *testing.T) {
	c, _, source, clock := newTestCache(t, 10*time.Second, time.Minute, 64)

	if got, err := c.Get(context.Background(), "k", constant("fresh", 0)); err != nil || got != "fresh" {
		t.Fatalf("Get = (%v, %v)", got, err)
	}
	clock.Advance(20 * time.Second)
	source.fail.Store(true)

	got, err := c.Get(context.Background(), "k", func(context.Context) (any, time.Duration, error) {
		return nil, 0, errors.New("database is down")
	})
	if err != nil {
		t.Fatalf("Get returned %v, want the stale value instead", err)
	}
	if got != "fresh" {
		t.Fatalf("got %v, want fresh", got)
	}
	if stats := c.Stats(); stats.ServedStale != 1 {
		t.Errorf("served_stale = %d, want 1", stats.ServedStale)
	}
}

func TestPermanentErrorIsNotMaskedByStaleValue(t *testing.T) {
	c, _, _, clock := newTestCache(t, 10*time.Second, time.Minute, 64)

	if _, err := c.Get(context.Background(), "k", constant("allowed", 0)); err != nil {
		t.Fatalf("Get: %v", err)
	}
	clock.Advance(20 * time.Second)

	_, err := c.Get(context.Background(), "k", func(context.Context) (any, time.Duration, error) {
		return nil, 0, Permanent(errors.New("no longer allowed"))
	})
	if err == nil {
		t.Fatal("Get returned no error: a definitive refusal must not be answered from a stale entry")
	}
}

func TestSizeLimitEvictsLeastRecentlyUsed(t *testing.T) {
	c, _, _, _ := newTestCache(t, time.Hour, time.Hour, 20)

	for i := range 25 {
		if _, err := c.Get(context.Background(), fmt.Sprintf("k%d", i), constant(i, 0)); err != nil {
			t.Fatalf("Get k%d: %v", i, err)
		}
	}
	stats := c.Stats()
	if stats.Entries != 20 {
		t.Errorf("entries = %d, want the limit 20", stats.Entries)
	}
	if stats.Evictions != 5 {
		t.Errorf("evictions = %d, want 5", stats.Evictions)
	}
	// k0 was the oldest; it must be rebuilt.
	var loads int
	if _, err := c.Get(context.Background(), "k0", func(context.Context) (any, time.Duration, error) {
		loads++
		return "rebuilt", 0, nil
	}); err != nil {
		t.Fatalf("Get k0: %v", err)
	}
	if loads != 1 {
		t.Error("evicted entry was not rebuilt")
	}
}

func TestVersionIsReadOncePerInterval(t *testing.T) {
	clock := testutil.NewClock()
	source := &fakeSource{err: errors.New("unavailable")}
	source.value.Store(3)
	versions := NewVersions(source, time.Second, discardLogger())
	versions.WithClock(clock.Now)

	for range 100 {
		got, err := versions.Current(context.Background(), "catalog")
		if err != nil {
			t.Fatalf("Current: %v", err)
		}
		if got != 3 {
			t.Fatalf("version = %d, want 3", got)
		}
	}
	if reads := source.reads.Load(); reads != 1 {
		t.Errorf("database read %d times for 100 lookups, want 1", reads)
	}

	clock.Advance(2 * time.Second)
	source.bump()
	if got, _ := versions.Current(context.Background(), "catalog"); got != 4 {
		t.Errorf("version = %d, want 4 after the interval", got)
	}
}

func TestVersionFallsBackWhenSourceFails(t *testing.T) {
	clock := testutil.NewClock()
	source := &fakeSource{err: errors.New("unavailable")}
	source.value.Store(5)
	versions := NewVersions(source, time.Second, discardLogger())
	versions.WithClock(clock.Now)

	if _, err := versions.Current(context.Background(), "catalog"); err != nil {
		t.Fatalf("Current: %v", err)
	}
	clock.Advance(2 * time.Second)
	source.fail.Store(true)

	got, err := versions.Current(context.Background(), "catalog")
	if err != nil {
		t.Fatalf("Current failed with %v, want the last known version", err)
	}
	if got != 5 {
		t.Errorf("version = %d, want the last known 5", got)
	}

	clock.Advance(2 * versionFallback)
	if _, err := versions.Current(context.Background(), "catalog"); err == nil {
		t.Error("the fallback must not last forever")
	}
}

// TestConcurrentUse hammers the cache from many goroutines while entries expire
// and invalidations happen. Run with -race.
func TestConcurrentUse(t *testing.T) {
	c, _, source, clock := newTestCache(t, 5*time.Millisecond, 50*time.Millisecond, 32)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 200 {
				key := fmt.Sprintf("k%d", i%7)
				_, err := c.Get(context.Background(), key, func(context.Context) (any, time.Duration, error) {
					return key, 0, nil
				})
				if err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				if i%16 == 0 {
					c.Invalidate(key)
					source.bump()
					clock.Advance(time.Millisecond)
				}
			}
		}(i)
	}
	wg.Wait()

	if entries := c.Stats().Entries; entries > 32 {
		t.Errorf("entries = %d, above the configured limit", entries)
	}
}
