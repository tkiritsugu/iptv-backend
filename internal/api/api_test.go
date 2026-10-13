// Package api tests drive the real HTTP stack: the router, the middleware, the
// service, the cache and SQLite. Nothing is stubbed, so a test that passes
// describes behaviour a client can actually observe.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"iptv/internal/auth"
	"iptv/internal/cache"
	"iptv/internal/limit"
	"iptv/internal/panel"
	"iptv/internal/password"
	"iptv/internal/seed"
	"iptv/internal/service"
	"iptv/internal/store"
	"iptv/internal/stream"
	"iptv/internal/testutil"
)

const (
	adminLogin    = "admin"
	adminPassword = "admin12345"
	viewerLogin   = "viewer"
	viewerPass    = "viewer12345"
	streamKey     = "test-key-test-key-test-key-test"
)

// syncWriter makes the log readable from an assertion while handlers run in
// other goroutines.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type envConfig struct {
	// loginBurst enables the failed-login limiter; zero disables it.
	loginBurst int
	versionTTL time.Duration
	streamTTL  time.Duration
	cacheTTL   time.Duration
	authTTL    time.Duration
}

type env struct {
	t       testing.TB
	server  *httptest.Server
	store   *store.Store
	clock   *testutil.Clock
	catalog *service.Channels
	signer  *stream.Signer
	logs    *syncWriter

	// A transport with a real connection pool, so a benchmark measures the
	// server and not the cost of dialing a socket per request.
	client *http.Client
}

// newEnv starts a service on a throwaway SQLite file, seeded with the same demo
// data the real binary creates on first start. All time in the test comes from
// one fake clock, so "a week later" costs nothing.
func newEnv(t testing.TB, cfg envConfig) *env {
	t.Helper()

	if cfg.versionTTL == 0 {
		cfg.versionTTL = time.Second
	}
	if cfg.streamTTL == 0 {
		cfg.streamTTL = 15 * time.Minute
	}
	if cfg.cacheTTL == 0 {
		cfg.cacheTTL = 5 * time.Minute
	}
	if cfg.authTTL == 0 {
		cfg.authTTL = time.Hour
	}

	clock := testutil.NewClock()
	logs := &syncWriter{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	dsn := store.DSN(filepath.Join(t.TempDir(), "api.db"))
	st, err := store.Open(t.Context(), dsn, logger, store.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// A cheap work factor: the format and the flow are under test, not the cost.
	hasher := password.New(password.MinimumIterations)

	if _, err := seed.Demo(t.Context(), st, hasher, logger); err != nil {
		t.Fatalf("seed: %v", err)
	}

	versions := cache.NewVersions(st, cfg.versionTTL, logger)
	versions.WithClock(clock.Now)

	signer, err := stream.New("http://stream.test", []byte(streamKey), cfg.streamTTL, true)
	if err != nil {
		t.Fatalf("stream.New: %v", err)
	}

	catalog := service.NewChannels(st, versions, cfg.cacheTTL, time.Minute, 512, signer, logger,
		service.WithClock(clock.Now))

	limiter := limit.New(cfg.loginBurst, time.Minute)
	authenticator, err := auth.New(st, versions, hasher, cfg.authTTL, cfg.authTTL, 512, limiter, logger)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	panelHandler, err := panel.New()
	if err != nil {
		t.Fatalf("panel.New: %v", err)
	}

	router := NewRouter(HandlerConfig{
		Channels:       catalog,
		Admin:          service.NewAdmin(st, hasher, logger, catalog, authenticator),
		Auth:           auth.NewMiddleware(authenticator, logger),
		AuthCache:      authenticator,
		DB:             st,
		Logger:         logger,
		RequestTimeout: 5 * time.Second,
		MaxBodyBytes:   64 << 10,
		Panel:          panelHandler,
	})

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return &env{
		t:       t,
		server:  server,
		store:   st,
		clock:   clock,
		catalog: catalog,
		signer:  signer,
		logs:    logs,
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 64,
			MaxConnsPerHost:     256,
		}},
	}
}

// ---- request helpers -------------------------------------------------------

type response struct {
	t      testing.TB
	status int
	header http.Header
	body   []byte
}

func (e *env) request(method, path, login, secret string, body any, headers ...string) response {
	e.t.Helper()

	var reader io.Reader
	if body != nil {
		switch payload := body.(type) {
		case string:
			reader = strings.NewReader(payload)
		default:
			encoded, err := json.Marshal(payload)
			if err != nil {
				e.t.Fatalf("marshal request body: %v", err)
			}
			reader = bytes.NewReader(encoded)
		}
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		e.t.Fatalf("NewRequest %s %s: %v", method, path, err)
	}
	if login != "" {
		req.SetBasicAuth(login, secret)
	}
	// A marshalled body is JSON by construction; a raw string is whatever the
	// test wants it to be, and "Content-Type:" means send no type at all.
	if _, raw := body.(string); body != nil && !raw {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, header := range headers {
		name, value, ok := strings.Cut(header, ":")
		if !ok {
			e.t.Fatalf("malformed header %q", header)
		}
		if value == "" {
			req.Header.Del(strings.TrimSpace(name))
			continue
		}
		req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}

	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read body of %s %s: %v", method, path, err)
	}
	return response{t: e.t, status: resp.StatusCode, header: resp.Header.Clone(), body: raw}
}

func (r response) wantStatus(want int) response {
	r.t.Helper()
	if r.status != want {
		r.t.Fatalf("%s: status = %d, want %d (body %s)", r.t.Name(), r.status, want, r.body)
	}
	return r
}

func (r response) decode(into any) {
	r.t.Helper()
	if err := json.Unmarshal(r.body, into); err != nil {
		r.t.Fatalf("decode response %s: %v", r.body, err)
	}
}

// envelope is the single error shape the API promises.
type envelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func (r response) errorBody() envelope {
	r.t.Helper()
	var body envelope
	r.decode(&body)
	if body.Error.Code == "" {
		r.t.Fatalf("error response has no code: %s", r.body)
	}
	return body
}

type channelView struct {
	ID        int64   `json:"id"`
	Number    int     `json:"number"`
	Name      string  `json:"name"`
	Available bool    `json:"available"`
	ExpiresAt *string `json:"expires_at"`
}

func (r response) channels() []channelView {
	r.t.Helper()
	var payload struct {
		Channels []channelView `json:"channels"`
		Count    int           `json:"count"`
		Mode     string        `json:"mode"`
	}
	r.decode(&payload)
	if payload.Count != len(payload.Channels) {
		r.t.Errorf("count = %d but %d channels in the list", payload.Count, len(payload.Channels))
	}
	return payload.Channels
}

func numbers(channels []channelView) []int {
	out := make([]int, 0, len(channels))
	for _, c := range channels {
		out = append(out, c.Number)
	}
	return out
}

// idOf looks up a seeded object by login or channel name.
func (e *env) userID(login string) int64 {
	e.t.Helper()
	resp := e.request(http.MethodGet, "/api/v1/admin/users", adminLogin, adminPassword, nil).wantStatus(http.StatusOK)
	var payload struct {
		Users []struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"users"`
	}
	resp.decode(&payload)
	for _, u := range payload.Users {
		if u.Login == login {
			return u.ID
		}
	}
	e.t.Fatalf("no seeded user %q", login)
	return 0
}

func (e *env) channelID(name string) int64 {
	e.t.Helper()
	resp := e.request(http.MethodGet, "/api/v1/admin/channels", adminLogin, adminPassword, nil).wantStatus(http.StatusOK)
	var payload struct {
		Channels []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"channels"`
	}
	resp.decode(&payload)
	for _, c := range payload.Channels {
		if c.Name == name {
			return c.ID
		}
	}
	e.t.Fatalf("no seeded channel %q", name)
	return 0
}

// ---- tests ----------------------------------------------------------------

func TestPublicEndpoints(t *testing.T) {
	e := newEnv(t, envConfig{})

	e.request(http.MethodGet, "/healthz", "", "", nil).wantStatus(http.StatusOK)
	e.request(http.MethodGet, "/readyz", "", "", nil).wantStatus(http.StatusOK)

	// An unknown path gets the same JSON shape as every other error, not the
	// plain text default of net/http.
	unknown := e.request(http.MethodGet, "/api/v1/nope", "", "", nil).wantStatus(http.StatusNotFound)
	if got := unknown.errorBody().Error.Code; got != "not_found" {
		t.Errorf("code = %q, want not_found", got)
	}
	if ct := unknown.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q, want json", ct)
	}
}

func TestAuthenticationRules(t *testing.T) {
	e := newEnv(t, envConfig{})

	t.Run("without credentials", func(t *testing.T) {
		resp := e.request(http.MethodGet, "/api/v1/channels", "", "", nil).wantStatus(http.StatusUnauthorized)
		if challenge := resp.header.Get("WWW-Authenticate"); !strings.Contains(challenge, "Basic realm=") {
			t.Errorf("WWW-Authenticate = %q, want a Basic challenge", challenge)
		}
		if resp.errorBody().Error.Code != "unauthorized" {
			t.Errorf("error code = %q", resp.errorBody().Error.Code)
		}
	})

	// The three ways in must all look identical to the caller: wrong password,
	// unknown login and a disabled account.
	cases := map[string][2]string{
		"wrong password": {viewerLogin, "not-the-password"},
		"unknown login":  {"nobody", viewerPass},
		"disabled user":  {"blocked", "blocked12345"},
		"empty password": {viewerLogin, ""},
	}
	for name, creds := range cases {
		t.Run(name, func(t *testing.T) {
			resp := e.request(http.MethodGet, "/api/v1/channels", creds[0], creds[1], nil).
				wantStatus(http.StatusUnauthorized)
			if resp.header.Get("WWW-Authenticate") == "" {
				t.Error("a 401 without WWW-Authenticate is not a usable challenge")
			}
		})
	}

	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)
}

// The seeded viewer has grants for channels 1-4 (4 is time limited) and none for
// 5; channel 6 is switched off.
func TestChannelListShowsOnlyWhatTheUserMayWatch(t *testing.T) {
	e := newEnv(t, envConfig{})

	channels := e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).
		wantStatus(http.StatusOK).channels()

	got := numbers(channels)
	want := []int{1, 2, 3, 4}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("channels = %v, want %v", got, want)
	}
	for _, c := range channels {
		if !c.Available {
			t.Errorf("channel %d is listed as unavailable in the default view", c.Number)
		}
	}
	// A time limited grant tells the client when it ends; an unlimited one says
	// null rather than pretending.
	for _, c := range channels {
		if c.Number == 4 && c.ExpiresAt == nil {
			t.Error("the time limited grant has no expires_at")
		}
		if c.Number == 1 && c.ExpiresAt != nil {
			t.Error("an unlimited grant reports an expiry")
		}
	}
}

func TestChannelListAllMode(t *testing.T) {
	e := newEnv(t, envConfig{})

	channels := e.request(http.MethodGet, "/api/v1/channels?all=true", viewerLogin, viewerPass, nil).
		wantStatus(http.StatusOK).channels()

	available := map[int]bool{}
	for _, c := range channels {
		available[c.Number] = c.Available
	}
	if available[5] {
		t.Error("channel 5 is available without a subscription")
	}
	if _, ok := available[6]; ok {
		t.Error("a switched off channel must not be listed at all")
	}
	if !available[1] || !available[4] {
		t.Errorf("channels the user may watch are missing: %v", available)
	}
}

func TestChannelListAdminSeesEverything(t *testing.T) {
	e := newEnv(t, envConfig{})

	channels := e.request(http.MethodGet, "/api/v1/channels", adminLogin, adminPassword, nil).
		wantStatus(http.StatusOK).channels()
	if len(channels) != 5 {
		t.Errorf("admin sees %d channels, want the 5 active ones", len(channels))
	}
}

func TestChannelListConditionalRequest(t *testing.T) {
	e := newEnv(t, envConfig{})

	first := e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)
	etag := first.header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on a cacheable list")
	}
	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil, "If-None-Match: "+etag).
		wantStatus(http.StatusNotModified)

	// After a change the validator must stop matching.
	e.request(http.MethodPut, fmt.Sprintf("/api/v1/admin/users/%d/subscriptions/%d", e.userID(viewerLogin), e.channelID("ТНТ")),
		adminLogin, adminPassword, map[string]any{"expires_at": nil}).wantStatus(http.StatusOK)

	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil, "If-None-Match: "+etag).
		wantStatus(http.StatusOK)
}

func TestPlaybackLinkIsSignedAndTimeLimited(t *testing.T) {
	e := newEnv(t, envConfig{})

	var playback struct {
		ChannelID int64     `json:"channel_id"`
		Number    int       `json:"number"`
		Name      string    `json:"name"`
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	resp := e.request(http.MethodGet, "/api/v1/channels/1/playback", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)
	resp.decode(&playback)

	if playback.ChannelID != 1 || playback.Name == "" {
		t.Errorf("unexpected payload: %+v", playback)
	}
	if !strings.HasPrefix(playback.URL, "http://stream.test/live/ch01/index.m3u8?") {
		t.Errorf("url = %q", playback.URL)
	}
	if !strings.Contains(playback.URL, "sig=") || !strings.Contains(playback.URL, "exp=") {
		t.Error("the link carries no signature")
	}
	if want := e.clock.Now().Add(15 * time.Minute); !playback.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", playback.ExpiresAt, want)
	}
	// What we hand out must pass the check an edge server would apply.
	if err := e.signer.Verify(playback.URL, "/live/ch01/index.m3u8", e.clock.Now()); err != nil {
		t.Errorf("issued link failed verification: %v", err)
	}
	if err := e.signer.Verify(playback.URL, "/live/ch02/index.m3u8", e.clock.Now()); err == nil {
		t.Error("the same signature is valid for another channel")
	}
	if cc := resp.header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("a per-user link must not be cached: Cache-Control = %q", cc)
	}
}

func TestPlaybackEdgeCases(t *testing.T) {
	e := newEnv(t, envConfig{})

	tests := []struct {
		path       string
		wantStatus int
		wantCode   string
	}{
		{"/api/v1/channels/5/playback", http.StatusForbidden, "subscription_required"},
		{"/api/v1/channels/6/playback", http.StatusNotFound, "channel_disabled"},
		{"/api/v1/channels/99999/playback", http.StatusNotFound, "channel_not_found"},
		{"/api/v1/channels/abc/playback", http.StatusBadRequest, "invalid_channel_id"},
		{"/api/v1/channels/0/playback", http.StatusBadRequest, "invalid_channel_id"},
		{"/api/v1/channels/-3/playback", http.StatusBadRequest, "invalid_channel_id"},
		{"/api/v1/channels/1e2/playback", http.StatusBadRequest, "invalid_channel_id"},
		{"/api/v1/channels/%20/playback", http.StatusBadRequest, "invalid_channel_id"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			resp := e.request(http.MethodGet, tc.path, viewerLogin, viewerPass, nil).wantStatus(tc.wantStatus)
			if got := resp.errorBody().Error.Code; got != tc.wantCode {
				t.Errorf("code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

func TestAdminEndpointsRequireTheAdminRole(t *testing.T) {
	e := newEnv(t, envConfig{})

	anonymous := e.request(http.MethodGet, "/api/v1/admin/users", "", "", nil).wantStatus(http.StatusUnauthorized)
	if anonymous.header.Get("WWW-Authenticate") == "" {
		t.Error("missing credentials must produce a challenge")
	}
	resp := e.request(http.MethodGet, "/api/v1/admin/users", viewerLogin, viewerPass, nil).
		wantStatus(http.StatusForbidden)
	if resp.errorBody().Error.Code != "forbidden" {
		t.Errorf("code = %q, want forbidden", resp.errorBody().Error.Code)
	}
	e.request(http.MethodGet, "/api/v1/admin/users", adminLogin, adminPassword, nil).wantStatus(http.StatusOK)
}

// TestAdminWriteTakesEffectImmediately is the regression test for the cache:
// a grant or a revoke has to be visible on the very next request, not after the
// cache TTL or the version poll.
func TestAdminWriteTakesEffectImmediately(t *testing.T) {
	e := newEnv(t, envConfig{})
	viewerID := e.userID(viewerLogin)
	tntID := e.channelID("ТНТ")
	path := fmt.Sprintf("/api/v1/channels/%d/playback", tntID)

	e.request(http.MethodGet, path, viewerLogin, viewerPass, nil).wantStatus(http.StatusForbidden)

	grant := fmt.Sprintf("/api/v1/admin/users/%d/subscriptions/%d", viewerID, tntID)
	e.request(http.MethodPut, grant, adminLogin, adminPassword, map[string]any{"expires_at": nil}).
		wantStatus(http.StatusOK)
	e.request(http.MethodGet, path, viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)

	e.request(http.MethodDelete, grant, adminLogin, adminPassword, nil).wantStatus(http.StatusNoContent)
	e.request(http.MethodGet, path, viewerLogin, viewerPass, nil).wantStatus(http.StatusForbidden)

	// Revoking twice is not an error pretending to be a change.
	e.request(http.MethodDelete, grant, adminLogin, adminPassword, nil).wantStatus(http.StatusNotFound)
}

// Another instance never calls InvalidateAll on this one: the version counter in
// the database is what makes the change visible.
func TestCachePicksUpAVersionChangeFromAnotherInstance(t *testing.T) {
	e := newEnv(t, envConfig{})
	tntID := e.channelID("ТНТ")
	viewerID := e.userID(viewerLogin)

	// Warm the cache with the current answer.
	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)

	// A write performed elsewhere: straight to the store, so the entry here is
	// neither invalidated nor expired.
	ctx := t.Context()
	if _, err := e.store.GrantSubscription(ctx, viewerID, tntID, nil); err != nil {
		t.Fatalf("GrantSubscription: %v", err)
	}
	e.clock.Advance(2 * time.Second) // long enough for the version poll

	channels := e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).
		wantStatus(http.StatusOK).channels()
	found := false
	for _, c := range channels {
		if c.Number == 5 {
			found = true
		}
	}
	if !found {
		t.Errorf("channel 5 is still missing after a write by another instance: %v", numbers(channels))
	}
}

// A grant that runs out must stop working by itself, without any write to bump
// the version counter and without anybody restarting the service.
func TestSubscriptionExpiryNeedsNoWrite(t *testing.T) {
	e := newEnv(t, envConfig{})
	ntvID := e.channelID("НТВ") // granted for seven days by the seed
	path := fmt.Sprintf("/api/v1/channels/%d/playback", ntvID)

	e.request(http.MethodGet, path, viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)
	e.clock.Advance(8 * 24 * time.Hour)
	e.request(http.MethodGet, path, viewerLogin, viewerPass, nil).wantStatus(http.StatusForbidden)

	// And it disappears from the list as well.
	for _, c := range e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).channels() {
		if c.Number == 4 {
			t.Error("the expired channel is still offered in the default view")
		}
	}
}

// Disabling an account is a security relevant change: it must take effect within
// the documented version interval even though verified credentials are cached.
func TestDisabledAccountStopsWithinTheVersionInterval(t *testing.T) {
	e := newEnv(t, envConfig{})
	viewerID := e.userID(viewerLogin)

	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)

	// The admin service clears the credential cache on this instance, so the
	// next request must already be rejected.
	e.request(http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", viewerID), adminLogin, adminPassword,
		map[string]any{"is_active": false}).wantStatus(http.StatusOK)
	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).wantStatus(http.StatusUnauthorized)
}

func TestAdminValidation(t *testing.T) {
	e := newEnv(t, envConfig{})
	jsonType := "Content-Type:application/json"

	tests := []struct {
		name       string
		method     string
		path       string
		body       any
		headers    []string
		wantStatus int
		wantCode   string
	}{
		{"short login", http.MethodPost, "/api/v1/admin/users", map[string]any{"login": "ab", "password": "longenough"}, []string{jsonType}, http.StatusBadRequest, "invalid_login"},
		{"login with a space", http.MethodPost, "/api/v1/admin/users", map[string]any{"login": "admin one", "password": "longenough"}, []string{jsonType}, http.StatusBadRequest, "invalid_login"},
		{"short password", http.MethodPost, "/api/v1/admin/users", map[string]any{"login": "fine", "password": "short"}, []string{jsonType}, http.StatusBadRequest, "invalid_password"},
		{"missing password", http.MethodPost, "/api/v1/admin/users", map[string]any{"login": "fine"}, []string{jsonType}, http.StatusBadRequest, "invalid_password"},
		{"typo in a field name", http.MethodPost, "/api/v1/admin/users", map[string]any{"login": "fine", "password": "longenough", "admim": true}, []string{jsonType}, http.StatusBadRequest, "invalid_json"},
		{"not json at all", http.MethodPost, "/api/v1/admin/users", "hello", []string{jsonType}, http.StatusBadRequest, "invalid_json"},
		{"empty body", http.MethodPost, "/api/v1/admin/users", "", []string{jsonType}, http.StatusBadRequest, "empty_body"},
		{"relative stream path", http.MethodPost, "/api/v1/admin/channels", map[string]any{"number": 90, "name": "x", "stream_path": "live/index.m3u8"}, []string{jsonType}, http.StatusBadRequest, "invalid_stream_path"},
		{"path traversal", http.MethodPost, "/api/v1/admin/channels", map[string]any{"number": 90, "name": "x", "stream_path": "/live/../secret"}, []string{jsonType}, http.StatusBadRequest, "invalid_stream_path"},
		{"path with a query", http.MethodPost, "/api/v1/admin/channels", map[string]any{"number": 90, "name": "x", "stream_path": "/live/a.m3u8?token=1"}, []string{jsonType}, http.StatusBadRequest, "invalid_stream_path"},
		{"number out of range", http.MethodPost, "/api/v1/admin/channels", map[string]any{"number": 0, "name": "x", "stream_path": "/live/a.m3u8"}, []string{jsonType}, http.StatusBadRequest, "invalid_number"},
		{"duplicate channel number", http.MethodPost, "/api/v1/admin/channels", map[string]any{"number": 1, "name": "copy", "stream_path": "/live/copy.m3u8"}, []string{jsonType}, http.StatusConflict, "already_exists"},
		{"unknown channel", http.MethodPatch, "/api/v1/admin/channels/424242", map[string]any{"is_active": false}, []string{jsonType}, http.StatusNotFound, "not_found"},
		{"unknown user", http.MethodGet, "/api/v1/admin/users/424242", nil, nil, http.StatusNotFound, "not_found"},
		{"grant to an unknown channel", http.MethodPut, "/api/v1/admin/users/2/subscriptions/424242", map[string]any{"expires_at": nil}, []string{jsonType}, http.StatusNotFound, "reference_not_found"},
		{"bad expires_at", http.MethodPut, "/api/v1/admin/users/2/subscriptions/1", map[string]any{"expires_at": "tomorrow"}, []string{jsonType}, http.StatusBadRequest, "invalid_json"},
		{"no content type on a write", http.MethodPost, "/api/v1/admin/channels", map[string]any{"number": 91, "name": "x", "stream_path": "/live/b.m3u8"}, []string{"Content-Type:"}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"cross origin write", http.MethodPost, "/api/v1/admin/channels", map[string]any{"number": 92, "name": "x", "stream_path": "/live/c.m3u8"}, []string{jsonType, "Origin:http://evil.example"}, http.StatusForbidden, "forbidden"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.request(tc.method, tc.path, adminLogin, adminPassword, tc.body, tc.headers...)
			if resp.status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", resp.status, tc.wantStatus, resp.body)
			}
			if tc.wantCode != "" {
				if got := resp.errorBody().Error.Code; got != tc.wantCode {
					t.Errorf("code = %q, want %q", got, tc.wantCode)
				}
			}
		})
	}
}

// An administrator who deactivates themselves would leave the system without a
// way in, so the request is refused instead.
func TestSelfLockoutIsRefused(t *testing.T) {
	e := newEnv(t, envConfig{})
	adminID := e.userID(adminLogin)
	jsonType := "Content-Type:application/json"

	resp := e.request(http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", adminID), adminLogin, adminPassword,
		map[string]any{"is_active": false}, jsonType).wantStatus(http.StatusConflict)
	if resp.errorBody().Error.Code != "self_lockout" {
		t.Errorf("code = %q, want self_lockout", resp.errorBody().Error.Code)
	}
	e.request(http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", adminID), adminLogin, adminPassword,
		map[string]any{"is_admin": false}, jsonType).wantStatus(http.StatusConflict)

	// Another administrator is allowed to do it.
	other := e.request(http.MethodPost, "/api/v1/admin/users", adminLogin, adminPassword,
		map[string]any{"login": "admin2", "password": "admin2pass", "is_admin": true}, jsonType).
		wantStatus(http.StatusCreated)
	var created struct {
		ID int64 `json:"id"`
	}
	other.decode(&created)
	e.request(http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", adminID), "admin2", "admin2pass",
		map[string]any{"is_active": false}, jsonType).wantStatus(http.StatusOK)
}

func TestHappyPathWrites(t *testing.T) {
	e := newEnv(t, envConfig{})
	jsonType := "Content-Type:application/json"

	created := e.request(http.MethodPost, "/api/v1/admin/channels", adminLogin, adminPassword,
		map[string]any{"number": 42, "name": "Новый", "stream_path": "/live/ch42/index.m3u8"}, jsonType).
		wantStatus(http.StatusCreated)
	var channel struct {
		ID         int64  `json:"id"`
		Number     int    `json:"number"`
		Name       string `json:"name"`
		StreamPath string `json:"stream_path"`
		Active     bool   `json:"is_active"`
	}
	created.decode(&channel)
	if channel.Number != 42 || !channel.Active || channel.StreamPath == "" {
		t.Fatalf("unexpected payload %+v", channel)
	}

	// The user the new channel is granted to can watch it immediately.
	viewerID := e.userID(viewerLogin)
	e.request(http.MethodPut, fmt.Sprintf("/api/v1/admin/users/%d/subscriptions/%d", viewerID, channel.ID),
		adminLogin, adminPassword, map[string]any{"expires_at": "2999-01-01T00:00:00Z"}, jsonType).
		wantStatus(http.StatusOK)
	playbackPath := fmt.Sprintf("/api/v1/channels/%d/playback", channel.ID)
	e.request(http.MethodGet, playbackPath, viewerLogin, viewerPass, nil).wantStatus(http.StatusOK)

	// Switching the channel off takes effect on the next request.
	updated := e.request(http.MethodPatch, fmt.Sprintf("/api/v1/admin/channels/%d", channel.ID), adminLogin, adminPassword,
		map[string]any{"is_active": false}, jsonType).wantStatus(http.StatusOK)
	if !bytes.Contains(updated.body, []byte(`"is_active":false`)) {
		t.Errorf("patch did not switch the channel off: %s", updated.body)
	}
	e.request(http.MethodGet, playbackPath, viewerLogin, viewerPass, nil).wantStatus(http.StatusNotFound)

	// The grant is visible with its expiry, and the password never appears in
	// any user representation.
	users := e.request(http.MethodGet, "/api/v1/admin/users", adminLogin, adminPassword, nil).wantStatus(http.StatusOK)
	if bytes.Contains(users.body, []byte("password")) {
		t.Errorf("the user list exposes a password field: %s", users.body)
	}
}

// Whatever goes wrong inside, the client gets a fixed message: no SQL text, no
// file paths, no stack traces. The detail belongs in the log. Here the schema is
// broken under the running service, which is the cheapest honest way to make a
// query fail.
func TestInternalFailuresStayOffTheWire(t *testing.T) {
	e := newEnv(t, envConfig{})

	if _, err := e.store.DB().ExecContext(t.Context(), "DROP TABLE subscriptions"); err != nil {
		t.Fatalf("DROP TABLE: %v", err)
	}
	resp := e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil)
	if resp.status < http.StatusInternalServerError {
		t.Fatalf("status = %d, want a server error when a query fails", resp.status)
	}
	body := resp.errorBody()
	if body.Error.Code != "internal_error" {
		t.Errorf("code = %q, want internal_error", body.Error.Code)
	}
	for _, leak := range []string{"sql", "SQLITE", "no such table", "store:", "subscriptions", ".db", "/src/"} {
		if strings.Contains(string(resp.body), leak) {
			t.Errorf("the error response leaks %q: %s", leak, resp.body)
		}
	}
	if body.Error.RequestID == "" || body.Error.RequestID != resp.header.Get("X-Request-Id") {
		t.Error("the error body must carry the request id the log also has")
	}
	if !strings.Contains(e.logs.String(), "no such table") {
		t.Errorf("the detail should be in the log even though the client does not see it:\n%s", e.logs.String())
	}
	if !strings.Contains(e.logs.String(), "request_id") {
		t.Error("a server error must be findable by its request id")
	}
}

// Credentials are the one thing that must never appear in the log.
func TestLogsNeverContainCredentials(t *testing.T) {
	e := newEnv(t, envConfig{})

	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, "definitely-wrong", nil)
	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil)
	e.request(http.MethodPost, "/api/v1/admin/users", adminLogin, adminPassword,
		map[string]any{"login": "logcheck", "password": "supersecret1"}, "Content-Type:application/json")
	e.request(http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", e.userID("logcheck")), adminLogin, adminPassword,
		map[string]any{"password": "changedsecret1"}, "Content-Type:application/json")

	logs := e.logs.String()
	for _, secret := range []string{"definitely-wrong", "supersecret1", "changedsecret1", viewerPass, adminPassword} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log contains %q:\n%s", secret, logs)
		}
	}
	for _, marker := range []string{"Authorization", "Basic ", "password_hash"} {
		if strings.Contains(logs, marker) {
			t.Errorf("the log contains %q:\n%s", marker, logs)
		}
	}
	// While still saying who called and what happened.
	if !strings.Contains(logs, "authentication failed") || !strings.Contains(logs, "admin action") {
		t.Errorf("the log lost the events it should report:\n%s", logs)
	}
}

func TestLoginFailuresAreRateLimited(t *testing.T) {
	e := newEnv(t, envConfig{loginBurst: 2})

	// Two failures are allowed, the third attempt is refused before the
	// expensive verification runs.
	for range 2 {
		e.request(http.MethodGet, "/api/v1/channels", viewerLogin, "wrong-password", nil).
			wantStatus(http.StatusUnauthorized)
	}
	blocked := e.request(http.MethodGet, "/api/v1/channels", viewerLogin, "wrong-password", nil)
	if blocked.status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", blocked.status)
	}
	if blocked.header.Get("Retry-After") == "" {
		t.Error("a 429 without Retry-After leaves the client guessing")
	}
	if blocked.errorBody().Error.Code != "too_many_requests" {
		t.Errorf("code = %q, want too_many_requests", blocked.errorBody().Error.Code)
	}
	// Even the correct password is refused while the window is open: the limit
	// protects the KDF, not the account.
	e.request(http.MethodGet, "/api/v1/channels", viewerLogin, viewerPass, nil).
		wantStatus(http.StatusTooManyRequests)
}

func TestAdminPanelIsServedAndProtected(t *testing.T) {
	e := newEnv(t, envConfig{})

	e.request(http.MethodGet, "/admin/", "", "", nil).wantStatus(http.StatusUnauthorized)
	e.request(http.MethodGet, "/admin/", viewerLogin, viewerPass, nil).wantStatus(http.StatusForbidden)

	page := e.request(http.MethodGet, "/admin/", adminLogin, adminPassword, nil).wantStatus(http.StatusOK)
	if !bytes.Contains(page.body, []byte("админ-панель")) {
		t.Error("the panel page did not render")
	}
	if csp := page.header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q, want a strict default", csp)
	}
	// The panel's script and stylesheet are same-origin files, not inline code.
	e.request(http.MethodGet, "/admin/app.js", adminLogin, adminPassword, nil).wantStatus(http.StatusOK)
	e.request(http.MethodGet, "/admin/style.css", adminLogin, adminPassword, nil).wantStatus(http.StatusOK)
	if !bytes.Contains(page.body, []byte("src=\"/admin/app.js\"")) {
		t.Error("the panel should reference its script instead of embedding it")
	}
}

func TestHeadRequestsAreHandled(t *testing.T) {
	e := newEnv(t, envConfig{})

	resp := e.request(http.MethodHead, "/api/v1/channels", viewerLogin, viewerPass, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", resp.status)
	}
	if len(resp.body) != 0 {
		t.Errorf("HEAD returned %d bytes of body", len(resp.body))
	}
}
