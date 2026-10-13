// Package auth turns HTTP Basic credentials into an authenticated principal.
//
// Rules implemented here:
//
//   - a request without credentials, with a wrong password or with an inactive
//     account gets 401 plus a WWW-Authenticate header, exactly as the task asks
//     for;
//   - an authenticated non-admin calling an admin route gets 403;
//   - verifying a password is deliberately expensive, so successful
//     authentications are cached per (login, password) for a few seconds. That
//     keeps the hot path cheap without ever caching a plaintext credential: the
//     cache key holds a keyed digest of the password, never the password;
//   - failed logins are rate limited per client address, and an unknown login
//     still pays the full hashing cost so that response times cannot be used to
//     find out which logins exist.
package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"iptv/internal/cache"
	"iptv/internal/httpx"
	"iptv/internal/limit"
	"iptv/internal/password"
	"iptv/internal/store"
)

// Principal is an authenticated caller.
type Principal struct {
	UserID int64
	Login  string
	Admin  bool
}

// ctxKey is unexported so no other package can collide with our context values.
type ctxKey int

const principalKey ctxKey = iota

// ErrUnauthorized reports missing or invalid credentials. It carries no detail
// about which part failed: the client does not need to know whether the login
// exists, the password is wrong or the account is switched off.
var ErrUnauthorized = errors.New("invalid credentials")

// Store is the part of the database this package needs.
type Store interface {
	UserByLogin(ctx context.Context, login string) (store.User, error)
	UpdateUser(ctx context.Context, id int64, patch store.UserPatch) (store.User, error)
}

// Authenticator verifies credentials.
type Authenticator struct {
	store    Store
	cache    *cache.Cache
	limiter  *limit.Limiter
	pwdKey   []byte
	log      *slog.Logger
}

// New builds an authenticator. The entries cache invalidates whenever the users
// data version changes, so disabling an account takes effect on every instance
// within the version poll interval.
func New(st Store, versions *cache.Versions, ttl time.Duration, staleTTL time.Duration, maxEntries int, limiter *limit.Limiter, log *slog.Logger) (*Authenticator, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("auth: fingerprint key: %w", err)
	}
	return &Authenticator{
		store:   st,
		cache:   cache.New(versions, "auth", store.VersionUsers, ttl, staleTTL, maxEntries, log),
		limiter: limiter,
		pwdKey:  key,
		log:     log,
	}, nil
}

// CacheStats exposes cache counters for the admin API.
func (a *Authenticator) CacheStats() cache.Stats { return a.cache.Stats() }

// Verify checks a login/password pair and returns the principal.
//
// It returns ErrUnauthorized for everything a client should be told is simply
// "not allowed", and a non-nil other error only for infrastructure problems.
func (a *Authenticator) Verify(ctx context.Context, login, secret string) (*Principal, error) {
	if login == "" || secret == "" {
		return nil, ErrUnauthorized
	}
	key := login + "|" + password.Fingerprint(a.pwdKey, secret)

	value, err := a.cache.Get(ctx, key, func(ctx context.Context) (any, time.Duration, error) {
		user, err := a.store.UserByLogin(ctx, login)
		if errors.Is(err, store.ErrNotFound) {
			// Spend the same CPU as for a real user, otherwise the response
			// time tells an attacker which logins exist.
			password.Burn()
			return nil, 0, cache.Permanent(ErrUnauthorized)
		}
		if err != nil {
			return nil, 0, err
		}
		ok, err := password.Verify(user.PasswordHash, secret)
		if err != nil {
			// A corrupt hash is our problem, not a reason to tell the client
			// anything specific.
			a.log.Error("password hash could not be verified", "user_id", user.ID, "error", err)
			return nil, 0, cache.Permanent(ErrUnauthorized)
		}
		if !ok || !user.Active {
			// A deactivated account must stop working now, not after the stale
			// window: Permanent keeps the cache from answering from memory.
			return nil, 0, cache.Permanent(ErrUnauthorized)
		}
		a.upgradeHash(ctx, user, secret)
		principal := &Principal{UserID: user.ID, Login: user.Login, Admin: user.Admin}
		return principal, 0, nil
	})
	if err != nil {
		return nil, err
	}
	cached, ok := value.(*Principal)
	if !ok {
		return nil, fmt.Errorf("auth: unexpected cache entry type %T", value)
	}
	// Copy: the cached entry is shared by concurrent requests and must not be
	// modified by any of them.
	p := *cached
	return &p, nil
}

// upgradeHash re-derives a password hash that was created with a weaker work
// factor. Doing it on a successful login — the only moment the plaintext is
// available — is how a work factor stays raiseable without a batch migration.
func (a *Authenticator) upgradeHash(ctx context.Context, user store.User, secret string) {
	if !password.NeedsUpgrade(user.PasswordHash) {
		return
	}
	hash, err := password.Hash(secret)
	if err != nil {
		a.log.Warn("could not upgrade password hash", "user_id", user.ID, "error", err)
		return
	}
	if _, err := a.store.UpdateUser(ctx, user.ID, store.UserPatch{PasswordHash: &hash}); err != nil {
		a.log.Warn("could not store upgraded password hash", "user_id", user.ID, "error", err)
	}
}

// Middleware enforces authentication on the routes it wraps.
type Middleware struct {
	auth *Authenticator
	log  *slog.Logger
}

// NewMiddleware builds the HTTP middleware.
func NewMiddleware(a *Authenticator, log *slog.Logger) *Middleware {
	return &Middleware{auth: a, log: log}
}

// Require wraps next with Basic authentication. onError renders the error
// envelope so this package stays independent of the response format.
func (m *Middleware) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, secret, ok := r.BasicAuth()
		if !ok {
			challenge(w)
			httpx.Error(w, r, http.StatusUnauthorized, "unauthorized",
				"authentication required: send an Authorization header with HTTP Basic credentials")
			return
		}
		login = strings.TrimSpace(login)

		ip := clientIP(r)
		if !m.auth.limiter.Allow(ip) {
			retry := int(m.auth.limiter.RetryAfter(ip).Seconds())
			if retry < 1 {
				retry = 1
			}
			m.log.Warn("login rate limit exceeded", "client", ip, "login", login, "retry_after_s", retry)
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			httpx.Error(w, r, http.StatusTooManyRequests, "too_many_requests",
				"too many failed login attempts, slow down")
			return
		}

		principal, err := m.auth.Verify(r.Context(), login, secret)
		if errors.Is(err, ErrUnauthorized) {
			// Only a failure costs a token: a client with correct credentials
			// can never exhaust its own budget.
			m.auth.limiter.Failure(ip)
			m.log.Info("authentication failed", "login", login, "client", ip)
			challenge(w)
			httpx.Error(w, r, http.StatusUnauthorized, "unauthorized",
				"invalid credentials or inactive account")
			return
		}
		if err != nil {
			m.log.Error("authentication failed with an internal error", "login", login, "error", err)
			httpx.Error(w, r, http.StatusServiceUnavailable, "unavailable",
				"authentication is temporarily unavailable")
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), principal)))
	})
}

// RequireAdmin wraps next with the admin role check. Combine after Require.
func (m *Middleware) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := FromContext(r.Context())
		if !ok {
			challenge(w)
			httpx.Error(w, r, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		if !principal.Admin {
			httpx.Error(w, r, http.StatusForbidden, "forbidden", "administrator access required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireJSON defends the admin write endpoints against cross-site request
// forgery. Browsers cannot send application/json from a cross-origin form, and
// when an Origin header is present it must match the request host, so a page on
// another site cannot reuse cached Basic credentials.
func (m *Middleware) RequireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json") {
			httpx.Error(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type",
				"request body must be application/json")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r) {
			httpx.Error(w, r, http.StatusForbidden, "forbidden", "cross-origin request rejected")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(origin string, r *http.Request) bool {
	o := strings.ToLower(strings.TrimSuffix(origin, "/"))
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return o == scheme+"://"+strings.ToLower(r.Host)
}

// clientIP returns the transport address of the caller. Proxy headers are
// intentionally not trusted: behind a reverse proxy this must be derived from
// the proxy configuration, otherwise a spoofed X-Forwarded-For would defeat the
// login limit. See README.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="IPTV API", charset="UTF-8"`)
}

// withPrincipal stores the authenticated caller in the context.
func withPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// FromContext returns the authenticated caller, if any.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey).(*Principal)
	return p, ok
}
