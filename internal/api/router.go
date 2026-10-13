// Package api is the HTTP surface: routing, middleware and handlers.
//
// Routing uses the method-and-pattern support of net/http.ServeMux from the
// standard library, so there is no router dependency. Handlers stay short: they
// parse and validate input, call the service and render the result. Everything
// that is not about this API — authentication, logging, panics, timeouts, body
// limits — lives in the middleware chain built here.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"iptv/internal/auth"
	"iptv/internal/cache"
	"iptv/internal/httpx"
	"iptv/internal/service"
)

// ReadStore is what the handlers need from the read service.
type ReadStore interface {
	List(ctx context.Context, userID int64, admin, includeUnavailable bool) (service.Rendered, error)
	Playback(ctx context.Context, userID int64, admin bool, channelID int64) (service.Playback, error)
}

// Pinger reports database health.
type Pinger interface {
	Ping(ctx context.Context) error
}

// StatsProvider exposes cache counters to the admin API.
type StatsProvider interface {
	CacheStats() cache.Stats
}

// HandlerConfig wires the router.
type HandlerConfig struct {
	Channels       *service.Channels
	Admin          *service.Admin
	Auth           *auth.Middleware
	AuthCache      StatsProvider
	DB             Pinger
	Logger         *slog.Logger
	RequestTimeout time.Duration
	MaxBodyBytes   int64

	// Panel serves the administrator UI; nil disables it.
	Panel http.Handler
}

type handler struct {
	cfg HandlerConfig
	log *slog.Logger
	now func() time.Time
}

// NewRouter builds the fully wrapped HTTP handler.
func NewRouter(cfg HandlerConfig) http.Handler {
	h := &handler{cfg: cfg, log: cfg.Logger, now: time.Now}
	mux := http.NewServeMux()

	// Public: probes only. They answer with a fixed word and no data.
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)

	protect := cfg.Auth.Require
	admin := func(fn http.HandlerFunc) http.Handler {
		return cfg.Auth.Require(cfg.Auth.RequireAdmin(fn))
	}
	adminWrite := func(fn http.HandlerFunc) http.Handler {
		return cfg.Auth.Require(cfg.Auth.RequireAdmin(cfg.Auth.RequireJSON(fn)))
	}

	mux.Handle("GET /api/v1/channels", protect(http.HandlerFunc(h.listChannels)))
	mux.Handle("GET /api/v1/channels/{id}/playback", protect(http.HandlerFunc(h.getPlayback)))

	mux.Handle("GET /api/v1/admin/users", admin(h.listUsers))
	mux.Handle("POST /api/v1/admin/users", adminWrite(h.createUser))
	mux.Handle("GET /api/v1/admin/users/{id}", admin(h.getUser))
	mux.Handle("PATCH /api/v1/admin/users/{id}", adminWrite(h.patchUser))

	mux.Handle("GET /api/v1/admin/channels", admin(h.listChannelsAdmin))
	mux.Handle("POST /api/v1/admin/channels", adminWrite(h.createChannel))
	mux.Handle("PATCH /api/v1/admin/channels/{id}", adminWrite(h.patchChannel))

	mux.Handle("GET /api/v1/admin/users/{id}/subscriptions", admin(h.listSubscriptions))
	mux.Handle("PUT /api/v1/admin/users/{id}/subscriptions/{channelId}", adminWrite(h.grantSubscription))
	mux.Handle("DELETE /api/v1/admin/users/{id}/subscriptions/{channelId}", admin(h.revokeSubscription))

	mux.Handle("GET /api/v1/admin/cache", admin(h.cacheStats))

	if cfg.Panel != nil {
		mux.Handle("GET /admin/", cfg.Auth.Require(cfg.Auth.RequireAdmin(cfg.Panel)))
		mux.Handle("GET /admin", http.RedirectHandler("/admin/", http.StatusMovedPermanently))
	}

	// Anything else is a JSON 404 instead of the plain text default, so clients
	// never have to parse two error formats.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "no such endpoint")
	})

	// Outermost first: recover must see panics from everything, the request id
	// must exist before anything logs, and the timeout must not be able to cut
	// short a response that is already being written.
	var chain http.Handler = mux
	chain = timeoutMiddleware(chain, cfg.RequestTimeout)
	chain = bodyLimitMiddleware(chain, cfg.MaxBodyBytes)
	chain = securityHeaders(chain)
	chain = accessLogMiddleware(chain, h.log)
	chain = requestIDMiddleware(chain)
	chain = recoverMiddleware(chain, h.log)
	return chain
}

// ---- endpoints -------------------------------------------------------------

func (h *handler) healthz(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports database reachability. Load balancers use it to take an
// instance out of rotation; it deliberately reveals nothing but ok/not ok.
func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.cfg.DB.Ping(ctx); err != nil {
		h.log.Error("readiness check failed", "error", err)
		httpx.JSON(w, r, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// listChannels serves GET /api/v1/channels.
func (h *handler) listChannels(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, r, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	all, err := parseMode(r.URL.Query().Get("all"))
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	rendered, err := h.cfg.Channels.List(r.Context(), principal.UserID, principal.Admin, all)
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	h.writeRendered(w, r, rendered)
}

// getPlayback serves GET /api/v1/channels/{id}/playback.
func (h *handler) getPlayback(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, r, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	channelID, err := parsePositiveID(r.PathValue("id"))
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	playback, err := h.cfg.Channels.Playback(r.Context(), principal.UserID, principal.Admin, channelID)
	if err != nil {
		// 403 and 404 are expected answers here, not incidents: log them as
		// information, everything else as an error.
		if stable := asStable(err); stable != nil && stable.Status < 500 {
			h.log.Info("playback denied", "user_id", principal.UserID, "channel_id", channelID, "code", stable.Code)
		}
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	h.log.Info("playback issued", "user_id", principal.UserID, "channel_id", playback.ChannelID)
	if err := httpx.JSON(w, r, http.StatusOK, playback); err != nil {
		h.log.Error("could not write playback response", "error", err)
	}
}

// writeRendered sends a cached body with conditional request support: a set-top
// box that polls the channel list can revalidate with If-None-Match and get an
// empty 304 instead of the payload.
func (h *handler) writeRendered(w http.ResponseWriter, r *http.Request, rendered service.Rendered) {
	head := w.Header()
	head.Set("Content-Type", "application/json; charset=utf-8")
	head.Set("ETag", rendered.ETag)
	// The response is per user and the request carries credentials, so no shared
	// cache may store it; the revalidation window is our own in-process cache.
	head.Set("Cache-Control", "private, no-store, must-revalidate")
	head.Set("X-Request-Id", httpx.RequestID(r.Context()))

	if matchETag(r.Header.Get("If-None-Match"), rendered.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(rendered.Body); err != nil {
		h.log.Error("could not write channel list", "error", err)
	}
}

// cacheStats exposes what the caches did — the quickest way to see, from the
// outside, whether the caching layer is doing its job.
func (h *handler) cacheStats(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{
		"channels": h.cfg.Channels.CacheStats(),
		"updated":  h.now().UTC().Format(time.RFC3339),
	}
	if h.cfg.AuthCache != nil {
		payload["auth"] = h.cfg.AuthCache.CacheStats()
	}
	if err := httpx.JSON(w, r, http.StatusOK, payload); err != nil {
		h.log.Error("could not write cache stats", "error", err)
	}
}

// ---- helpers ---------------------------------------------------------------

// parseMode reads the optional "all" query parameter. A value we do not
// understand is rejected instead of silently ignored: a client that believes it
// asked for one mode should not quietly receive another.
func parseMode(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false", "no":
		return false, nil
	case "1", "true", "yes":
		return true, nil
	default:
		return false, httpx.NewError(http.StatusBadRequest, "invalid_query",
			`the "all" parameter must be true or false`)
	}
}

// parsePositiveID turns a path segment into an id. Anything that is not a plain
// positive integer is a 400, which includes 0, negatives, spaces and "1abc".
func parsePositiveID(raw string) (int64, error) {
	if raw == "" {
		return 0, httpx.NewError(http.StatusBadRequest, "invalid_channel_id", "channel id is required")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, httpx.NewError(http.StatusBadRequest, "invalid_channel_id",
			"channel id must be a positive integer")
	}
	return id, nil
}

// parseIDValue parses an id coming from an admin path.
func parseIDValue(raw, name string) (int64, error) {
	id, err := parsePositiveID(raw)
	if err != nil {
		var stable *httpx.ErrorStable
		if errors.As(err, &stable) {
			return 0, httpx.NewError(stable.Status, "invalid_"+name, name+" must be a positive integer")
		}
		return 0, err
	}
	return id, nil
}

func matchETag(header, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	want := normalizeETag(etag)
	for candidate := range strings.SplitSeq(header, ",") {
		c := normalizeETag(candidate)
		// A weak validator matches regardless of strength; "*" matches any.
		if c == "*" || c == want {
			return true
		}
	}
	return false
}

func normalizeETag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	return strings.Trim(s, `"`)
}

func asStable(err error) *httpx.ErrorStable {
	var stable *httpx.ErrorStable
	if errors.As(err, &stable) {
		return stable
	}
	return nil
}
