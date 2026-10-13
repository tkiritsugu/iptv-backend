// Package service holds the business rules: what a user is allowed to see, what
// a playback link costs, what an administrator may change.
//
// It sits between the HTTP layer and the store. The store knows SQL, the
// handlers know HTTP, and neither of them decides whether a subscription counts.
//
// Reads that a set-top box performs on the hot path go through an in-process
// cache. Two things are cached, both keyed by data that changes rarely:
//
//   - the rendered channel list per user (bytes plus ETag) — marshalling a list
//     a million times a second buys nothing, so it is marshalled once;
//   - the access decision for one channel, not the link: the link carries a
//     fresh expiry, so it is rebuilt per request from the cached decision with
//     one HMAC.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"iptv/internal/cache"
	"iptv/internal/httpx"
	"iptv/internal/store"
	"iptv/internal/stream"
)

// Stable API errors of the read side.
var (
	// ErrChannelNotFound: no such channel.
	ErrChannelNotFound = httpx.NewError(404, "channel_not_found", "channel does not exist")
	// ErrChannelDisabled: the channel exists but does not broadcast. Reported as
	// 404 like a missing channel, as the task asks for.
	ErrChannelDisabled = httpx.NewError(404, "channel_disabled", "channel is not available")
	// ErrSubscriptionRequired: the channel is fine, the user just has no valid
	// grant. 403 rather than 404 because the channel is in the user's own
	// package list and hiding it would be a lie the client cannot act on: the
	// set-top box needs to show "subscribe to watch".
	ErrSubscriptionRequired = httpx.NewError(403, "subscription_required", "this channel requires an active subscription")
)

// ChannelView is one entry of the channel list.
type ChannelView struct {
	ID     int64  `json:"id"`
	Number int    `json:"number"`
	Name   string `json:"name"`
	// Available is true when the playback endpoint would hand out a link. In
	// the default mode every entry is available; the field stays in the shape so
	// clients can treat both modes identically.
	Available bool `json:"available"`
	// ExpiresAt is when the grant stops working; null means unlimited.
	ExpiresAt *time.Time `json:"expires_at"`
}

// ChannelListView is the body of GET /api/v1/channels.
type ChannelListView struct {
	Channels []ChannelView `json:"channels"`
	Count    int           `json:"count"`
	// Mode echoes which of the two views was returned: "available" (the
	// default, only what the user may watch) or "all" (every active channel
	// with its availability).
	Mode string `json:"mode"`
}

// Playback is the body of GET /api/v1/channels/{id}/playback.
type Playback struct {
	ChannelID int64     `json:"channel_id"`
	Number    int       `json:"number"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Rendered is a cached response body with its validator.
type Rendered struct {
	Body []byte
	ETag string
}

// AccessStore is the read side of the store.
type AccessStore interface {
	AccessForUser(ctx context.Context, userID int64, admin bool) ([]store.ChannelAccess, error)
	Access(ctx context.Context, userID, channelID int64, admin bool) (store.ChannelAccess, error)
}

// Channels serves the two read endpoints.
type Channels struct {
	store   AccessStore
	cache   *cache.Cache
	ttl     time.Duration
	signer  *stream.Signer
	log     *slog.Logger
	now     func() time.Time
}

// Option tweaks the service.
type Option func(*Channels)

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) Option {
	return func(c *Channels) { c.now = now }
}

// NewChannels builds the read service.
func NewChannels(st AccessStore, versions *cache.Versions, ttl, staleTTL time.Duration, maxEntries int, signer *stream.Signer, log *slog.Logger, opts ...Option) *Channels {
	c := &Channels{
		store:  st,
		cache:  cache.New(versions, "channels", store.VersionCatalog, ttl, staleTTL, maxEntries, log),
		ttl:    ttl,
		signer: signer,
		log:    log,
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// CacheStats exposes counters for the admin API.
func (c *Channels) CacheStats() cache.Stats { return c.cache.Stats() }

// InvalidateAll drops every cached read. An admin write calls it on the
// instance that performed the change so the effect is immediate locally, while
// the other instances converge within one version poll. Writes are rare, so
// throwing the whole local cache away is cheaper and safer than working out
// which keys were affected.
func (c *Channels) InvalidateAll() { c.cache.InvalidateAll() }

// List returns the rendered channel list for one user.
//
// includeUnavailable selects the mode: false returns only the channels the user
// may watch, true returns every active channel with an availability flag. Both
// are cached, keyed by user and mode.
func (c *Channels) List(ctx context.Context, userID int64, admin, includeUnavailable bool) (Rendered, error) {
	mode := "available"
	if includeUnavailable {
		mode = "all"
	}
	key := "channels:" + strconv.FormatInt(userID, 10) + ":" + btoa(admin) + ":" + mode

	value, err := c.cache.Get(ctx, key, func(ctx context.Context) (any, time.Duration, error) {
		access, err := c.store.AccessForUser(ctx, userID, admin)
		if err != nil {
			return nil, 0, err
		}
		view := ChannelListView{Mode: mode, Channels: make([]ChannelView, 0, len(access))}
		for _, a := range access {
			if !a.Available && !includeUnavailable {
				continue
			}
			view.Channels = append(view.Channels, ChannelView{
				ID:        a.Channel.ID,
				Number:    a.Channel.Number,
				Name:      a.Channel.Name,
				Available: a.Available,
				ExpiresAt: a.ExpiresAt,
			})
		}
		view.Count = len(view.Channels)

		body, err := json.Marshal(view)
		if err != nil {
			return nil, 0, fmt.Errorf("service: render channels: %w", err)
		}
		// A cached body must be byte-identical for the ETag to be meaningful, so
		// nothing request scoped (server time, request id) may appear in it.
		rendered := Rendered{Body: body, ETag: etag(body)}
		// The entry may not outlive the earliest grant expiry in it: nothing
		// polls for a subscription running out, the TTL simply ends when the
		// data stops being true.
		return rendered, c.ttlUntil(store.NextExpiry(access)), nil
	})
	if err != nil {
		return Rendered{}, err
	}
	rendered, ok := value.(Rendered)
	if !ok {
		return Rendered{}, fmt.Errorf("service: unexpected cache entry type %T", value)
	}
	return rendered, nil
}

// accessEntry is the cached part of a playback request: the channel and whether
// the user may watch it. The URL itself is never cached because it carries a
// per-request expiry.
type accessEntry struct {
	channel store.Channel
	err     error
}

// Playback returns a signed playback link for one channel.
func (c *Channels) Playback(ctx context.Context, userID int64, admin bool, channelID int64) (Playback, error) {
	key := "playback:" + strconv.FormatInt(userID, 10) + ":" + btoa(admin) + ":" + strconv.FormatInt(channelID, 10)

	value, err := c.cache.Get(ctx, key, func(ctx context.Context) (any, time.Duration, error) {
		access, err := c.store.Access(ctx, userID, channelID, admin)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return accessEntry{err: ErrChannelNotFound}, 0, nil
		case errors.Is(err, store.ErrDisabled):
			return accessEntry{err: ErrChannelDisabled}, 0, nil
		case errors.Is(err, store.ErrNoSubscription):
			return accessEntry{err: ErrSubscriptionRequired}, 0, nil
		case err != nil:
			return nil, 0, err
		}
		// A negative decision is cached like a positive one: only a write can
		// change it, and every write bumps the catalog version.
		return accessEntry{channel: access.Channel}, c.accessTTL(access), nil
	})
	if err != nil {
		return Playback{}, err
	}
	entry, ok := value.(accessEntry)
	if !ok {
		return Playback{}, fmt.Errorf("service: unexpected cache entry type %T", value)
	}
	if entry.err != nil {
		return Playback{}, entry.err
	}

	link, expires, err := c.signer.Playback(entry.channel.StreamPath, c.now())
	if err != nil {
		c.log.Error("could not build playback url", "channel_id", entry.channel.ID, "error", err)
		return Playback{}, httpx.NewError(500, "internal_error", "the request could not be completed")
	}
	return Playback{
		ChannelID: entry.channel.ID,
		Number:    entry.channel.Number,
		Name:      entry.channel.Name,
		URL:       link,
		ExpiresAt: expires.UTC(),
	}, nil
}

// accessTTL shortens the cache lifetime of a decision that is about to stop
// being true on its own: a grant that runs out in forty seconds must not be
// cached for five minutes.
func (c *Channels) accessTTL(access store.ChannelAccess) time.Duration {
	if access.ExpiresAt == nil {
		return 0
	}
	return c.ttlUntil(*access.ExpiresAt)
}

// ttlUntil turns "the moment this data stops being true" into a cache TTL:
// the zero time means "nothing expires on its own, use the default", a moment
// beyond the default is clamped to it, and a moment already in the past keeps
// the entry for one second only.
func (c *Channels) ttlUntil(until time.Time) time.Duration {
	if until.IsZero() {
		return 0
	}
	left := until.Sub(c.now())
	switch {
	case left <= 0:
		return time.Second
	case left >= c.ttl:
		return 0
	default:
		return left
	}
}

func etag(body []byte) string {
	sum := sha256.Sum256(body)
	return `W/"` + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:16]) + `"`
}

func btoa(b bool) string {
	if b {
		return "a"
	}
	return "u"
}
