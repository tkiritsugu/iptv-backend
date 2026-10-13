// Package cache is a small response cache built for one shape of load: a huge
// number of mostly identical reads that must not reach the database.
//
// Invalidation is version based rather than TTL based. The store keeps a
// monotonically increasing counter per group of data (users, catalog) and every
// write bumps it. A cache entry records the counter value it was built from: if
// the counter has not moved, the entry is still correct no matter how long the
// process has been running. That makes invalidation work across several
// instances of the service without any inter-instance messaging — a write in
// instance A bumps the counter, instance B notices within VersionTTL.
//
// The remaining job of the TTL is to cover changes that are not writes, above
// all a subscription reaching its expiry moment; callers therefore hand the
// cache a TTL that ends when their data stops being true.
//
// Two further properties matter for the "million set-top boxes reboot at once"
// scenario: concurrent misses on the same key are collapsed into a single load,
// and an entry past its TTL is still served when the database fails, so a short
// database outage degrades into stale data instead of a storm of 503s.
package cache

import (
	"container/list"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// VersionSource reads the current version of a data group. Implemented by
// *store.Store.
type VersionSource interface {
	Version(ctx context.Context, key string) (int64, error)
}

// versionFallback bounds how long the last known version is trusted when the
// database cannot be reached.
const versionFallback = time.Minute

// Versions caches the data version counters and is shared by every cache in the
// process. Without it each cached read would still cost one SELECT, which is
// precisely the database traffic we are trying to remove.
type Versions struct {
	src VersionSource
	ttl time.Duration
	log *slog.Logger

	now func() time.Time

	mu       sync.Mutex
	entries  map[string]*versionCall
	inflight map[string]*versionCall
}

type versionCall struct {
	wg      sync.WaitGroup
	value   int64
	err     error
	fetched time.Time
}

// NewVersions returns a version reader that queries the database at most once
// per ttl per data group, collapsing concurrent readers.
func NewVersions(src VersionSource, ttl time.Duration, log *slog.Logger) *Versions {
	return &Versions{
		src:      src,
		ttl:      ttl,
		log:      log,
		now:      time.Now,
		entries:  make(map[string]*versionCall),
		inflight: make(map[string]*versionCall),
	}
}

// WithClock replaces the time source of the version reader, for tests.
func (v *Versions) WithClock(now func() time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.now = now
}

// Current returns the version of a data group.
//
// If the database is unreachable the last known version is reused for a short
// while: cache entries then age out on their TTL instead of the API returning
// errors for a bookkeeping value.
func (v *Versions) Current(ctx context.Context, group string) (int64, error) {
	v.mu.Lock()
	if c := v.entries[group]; c != nil && c.err == nil && v.now().Before(c.fetched.Add(v.ttl)) {
		value := c.value
		v.mu.Unlock()
		return value, nil
	}
	if c := v.inflight[group]; c != nil {
		v.mu.Unlock()
		c.wg.Wait()
		return c.value, c.err
	}
	c := &versionCall{}
	c.wg.Add(1)
	v.inflight[group] = c
	v.mu.Unlock()

	value, err := v.src.Version(ctx, group)

	v.mu.Lock()
	delete(v.inflight, group)
	c.value, c.err, c.fetched = value, err, v.now()
	if err == nil {
		v.entries[group] = c
	}
	v.mu.Unlock()
	c.wg.Done()

	if err != nil {
		v.mu.Lock()
		last := v.entries[group]
		v.mu.Unlock()
		if last != nil && v.now().Before(last.fetched.Add(v.ttl+versionFallback)) {
			v.log.Warn("data version lookup failed, reusing last known version",
				"group", group, "version", last.value, "error", err)
			return last.value, nil
		}
		return 0, err
	}
	return value, nil
}

// permanentError marks a loader failure as a definitive answer rather than an
// infrastructure problem, so the cache refuses to fall back to a stale entry.
// Authenticating a user whose account has been switched off is the case that
// matters: "no longer allowed" must never be answered from a stale entry.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err so Get does not serve a stale value for it.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// IsPermanent reports whether err was produced by Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// Loader builds a value on a cache miss. A ttl <= 0 means "use the cache
// default"; a caller whose data expires on its own — a time limited
// subscription — returns the shorter lifetime so an entry cannot outlive the
// fact it describes.
type Loader func(ctx context.Context) (val any, ttl time.Duration, err error)

// Cache stores values guarded by a version counter, with an LRU size limit.
type Cache struct {
	versions   *Versions
	name       string
	group      string
	ttl        time.Duration
	staleTTL   time.Duration
	maxEntries int
	log        *slog.Logger

	now func() time.Time

	mu       sync.Mutex
	entries  map[string]*list.Element
	order    *list.List // front = most recently used
	inflight map[string]*loadCall
	stats    Stats
}

// Stats describes cache behaviour and is exposed through the admin API.
type Stats struct {
	Hits         int64  `json:"hits"`
	Misses       int64  `json:"misses"`
	Collisions   int64  `json:"collisions"` // requests that waited for someone else's load
	Loads        int64  `json:"loads"`
	LoadErrors   int64  `json:"load_errors"`
	ServedStale  int64  `json:"served_stale"`
	Evictions    int64  `json:"evictions"`
	Entries      int    `json:"entries"`
	MaxEntries   int    `json:"max_entries"`
	TTLSeconds   int    `json:"ttl_seconds"`
	GroupVersion string `json:"-"`
}

type entry struct {
	key       string
	value     any
	version   int64
	expiresAt time.Time
	hardAt    time.Time // expiresAt + stale window: last moment it may be served
}

type loadCall struct {
	wg  sync.WaitGroup
	val any
	ttl time.Duration
	err error
}

// Option tweaks a cache.
type Option func(*Cache)

// WithClock replaces the time source, which makes expiry testable.
func WithClock(now func() time.Time) Option {
	return func(c *Cache) { c.now = now }
}

// New creates a cache whose entries are invalidated by a version counter.
func New(versions *Versions, name, group string, ttl, staleTTL time.Duration, maxEntries int, log *slog.Logger, opts ...Option) *Cache {
	c := &Cache{
		versions:   versions,
		name:       name,
		group:      group,
		ttl:        ttl,
		staleTTL:   staleTTL,
		maxEntries: maxEntries,
		log:        log,
		now:        time.Now,
		entries:    make(map[string]*list.Element),
		order:      list.New(),
		inflight:   make(map[string]*loadCall),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Get returns the cached value for key, building it through load when needed.
// The entry is valid while the data version is unchanged and its TTL has not
// passed; beyond the TTL, up to the stale window, it may still be served if the
// reload fails.
func (c *Cache) Get(ctx context.Context, key string, load Loader) (any, error) {
	version, versionErr := c.versions.Current(ctx, c.group)

	c.mu.Lock()
	if el, ok := c.entries[key]; ok {
		e := el.Value.(*entry)
		unchanged := versionErr == nil && e.version == version
		// When the version could not be read we behave as if nothing changed;
		// the TTL and the stale window keep us honest.
		if (versionErr != nil || unchanged) && c.now().Before(e.expiresAt) {
			// Copy the value while holding the lock: another goroutine may be
			// replacing this entry's fields right now.
			value := e.value
			c.order.MoveToFront(el)
			c.stats.Hits++
			c.mu.Unlock()
			return value, nil
		}
		c.stats.Misses++
	} else {
		c.stats.Misses++
	}
	c.mu.Unlock()

	value, ttl, err := c.doLoad(ctx, key, load)
	if err != nil {
		if IsPermanent(err) {
			return nil, err
		}
		c.mu.Lock()
		if el, ok := c.entries[key]; ok && c.now().Before(el.Value.(*entry).hardAt) {
			value := el.Value.(*entry).value
			c.stats.ServedStale++
			c.mu.Unlock()
			c.log.Warn("serving stale entry after failed reload",
				"cache", c.name, "error", err)
			return value, nil
		}
		c.mu.Unlock()
		return nil, err
	}

	c.mu.Lock()
	expiresAt := c.now().Add(ttl)
	if el, ok := c.entries[key]; ok {
		e := el.Value.(*entry)
		e.value, e.version = value, version
		e.expiresAt, e.hardAt = expiresAt, expiresAt.Add(c.staleTTL)
		c.order.MoveToFront(el)
	} else {
		e := &entry{
			key:       key,
			value:     value,
			version:   version,
			expiresAt: expiresAt,
			hardAt:    expiresAt.Add(c.staleTTL),
		}
		c.entries[key] = c.order.PushFront(e)
		for c.order.Len() > c.maxEntries {
			back := c.order.Back()
			c.order.Remove(back)
			delete(c.entries, back.Value.(*entry).key)
			c.stats.Evictions++
		}
	}
	c.mu.Unlock()
	return value, nil
}

// doLoad runs the loader for key, collapsing concurrent misses onto one call.
// The ttl travels with the value instead of living on the cache, so parallel
// loads of different keys cannot mix their lifetimes.
func (c *Cache) doLoad(ctx context.Context, key string, fn Loader) (val any, ttl time.Duration, err error) {
	c.mu.Lock()
	if cc, ok := c.inflight[key]; ok {
		c.stats.Collisions++
		c.mu.Unlock()
		cc.wg.Wait()
		return cc.val, cc.ttl, cc.err
	}
	cc := &loadCall{}
	cc.wg.Add(1)
	c.inflight[key] = cc
	c.mu.Unlock()

	// The in-flight slot has to be released on every path, including a panic in
	// the loader. A slot that is never deleted answers every later request for
	// that key with the first result for the lifetime of the process — which is
	// exactly how a fresh grant once kept returning "subscription required".
	var loaded bool
	defer func() {
		if !loaded && err == nil {
			val, ttl, err = nil, c.ttl, errLoadAborted
		}
		c.mu.Lock()
		delete(c.inflight, key)
		c.stats.Loads++
		if err != nil {
			c.stats.LoadErrors++
		}
		c.mu.Unlock()
		cc.val, cc.ttl, cc.err = val, ttl, err
		cc.wg.Done()
	}()

	val, ttl, err = fn(ctx)
	loaded = true
	if ttl <= 0 {
		ttl = c.ttl
	}
	return val, ttl, err
}

// errLoadAborted reports a loader that did not return normally.
var errLoadAborted = errors.New("cache: load did not complete")

// Stats returns a snapshot of the counters.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Entries = c.order.Len()
	s.MaxEntries = c.maxEntries
	s.TTLSeconds = int(c.ttl.Seconds())
	return s
}

// Invalidate drops a single entry, so a write on this instance takes effect
// immediately instead of after the next version poll.
func (c *Cache) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.order.Remove(el)
		delete(c.entries, el.Value.(*entry).key)
	}
}

// InvalidateAll empties the cache.
func (c *Cache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*list.Element)
	c.order.Init()
}
