package api

import (
	"net/http"
	"testing"
	"time"
)

// These benchmarks measure the whole HTTP stack over a real socket — routing,
// middleware, Basic Auth, service, cache, JSON — so the difference between the
// cached and the uncached path is the difference a set-top box would feel.
// They are not a network benchmark: no TLS, no real clients, one process.
//
//   ./hack/go.sh test -run '^$' -bench . -benchtime 2s ./internal/api/

func BenchmarkChannelListCached(b *testing.B) {
	e := newEnv(b, envConfig{})
	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil)
	}
}

// The revalidation path a polling client uses: the answer is a 304 with no body.
func BenchmarkChannelListRevalidate(b *testing.B) {
	e := newEnv(b, envConfig{})
	first := e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)
	etag := first.header.Get("ETag")

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil, "If-None-Match: "+etag)
	}
}

// The same request with the cache emptied on every iteration: this is what the
// service would cost without the cache, one catalog query per request.
func BenchmarkChannelListUncached(b *testing.B) {
	e := newEnv(b, envConfig{})
	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		e.catalog.InvalidateAll()
		e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil)
	}
}

// With dozens of connections in flight the number is closer to what an instance
// actually serves, because the request rate is no longer limited by the
// round-trip time of one sequential client.
func BenchmarkChannelListParallel(b *testing.B) {
	e := newEnv(b, envConfig{})
	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil)
		}
	})
}

func BenchmarkPlaybackParallel(b *testing.B) {
	e := newEnv(b, envConfig{})
	e.request(http.MethodGet, "/api/v1/channels/1/playback", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			e.request(http.MethodGet, "/api/v1/channels/1/playback", viewerLogin, viewerPass, nil)
		}
	})
}

// Without the cache every request would run the catalog query; this is the same
// load with InvalidateAll called on each iteration.
func BenchmarkChannelListParallelUncached(b *testing.B) {
	e := newEnv(b, envConfig{})
	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			e.catalog.InvalidateAll()
			e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil)
		}
	})
}

func BenchmarkPlaybackCached(b *testing.B) {
	e := newEnv(b, envConfig{})
	e.request(http.MethodGet, "/api/v1/channels/1/playback", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		e.request(http.MethodGet, "/api/v1/channels/1/playback", viewerLogin, viewerPass, nil)
	}
}

// The first request after a cache invalidation also pays the version poll and a
// catalog query; with a million boxes arriving at once, only one of them per
// instance does this work, the rest join the same load.
func BenchmarkChannelListAfterInvalidation(b *testing.B) {
	e := newEnv(b, envConfig{versionTTL: time.Nanosecond})
	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		e.catalog.InvalidateAll()
		e.clock.Advance(time.Millisecond)
		e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil)
	}
}
