package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"iptv/internal/auth"
	"iptv/internal/httpx"
	"iptv/internal/service"
	"iptv/internal/store"
)

// The admin API is JSON in, JSON out, and the panel is a thin client of it —
// one implementation of "what may an administrator change" instead of two.
//
// Requests that carry a body additionally pass the RequireJSON check: the body
// must be application/json and a browser Origin header, when present, must match
// the host. That is what makes cached Basic credentials unusable from a page on
// another site.

type createUserRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
	Active   *bool  `json:"is_active"`
	IsAdmin  *bool  `json:"is_admin"`
}

type patchUserRequest struct {
	Login    *string `json:"login"`
	Password *string `json:"password"`
	Active   *bool   `json:"is_active"`
	IsAdmin  *bool   `json:"is_admin"`
}

type createChannelRequest struct {
	Number     int    `json:"number"`
	Name       string `json:"name"`
	StreamPath string `json:"stream_path"`
	Active     *bool  `json:"is_active"`
}

type patchChannelRequest struct {
	Number     *int    `json:"number"`
	Name       *string `json:"name"`
	StreamPath *string `json:"stream_path"`
	Active     *bool   `json:"is_active"`
}

type grantRequest struct {
	// ExpiresAt is an RFC 3339 timestamp, or null for an unlimited grant. A
	// timestamp in the past is accepted and means "expired already".
	ExpiresAt *time.Time `json:"expires_at"`
}

func (h *handler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.cfg.Admin.ListUsers(r.Context())
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	if users == nil {
		users = []store.User{}
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"users": users, "count": len(users)})
}

func (h *handler) getUser(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDValue(r.PathValue("id"), "user_id")
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	// The service has no single-user read for the admin view; the list is small
	// and the store lookup is indexed, so this stays a store concern.
	users, err := h.cfg.Admin.ListUsers(r.Context())
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	for _, u := range users {
		if u.ID == id {
			httpx.JSON(w, r, http.StatusOK, u)
			return
		}
	}
	httpx.Error(w, r, http.StatusNotFound, "not_found", "the requested object does not exist")
}

func (h *handler) createUser(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[createUserRequest](h, w, r)
	if !ok {
		return
	}
	user, err := h.cfg.Admin.CreateUser(r.Context(), service.CreateUserInput{
		Login:    req.Login,
		Password: req.Password,
		Active:   deref(req.Active, true),
		Admin:    deref(req.IsAdmin, false),
	})
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	h.log.Info("admin action", "action", "create_user", "admin_id", adminID(r), "user_id", user.ID)
	if err := httpx.JSON(w, r, http.StatusCreated, user); err != nil {
		h.log.Error("could not write response", "error", err)
	}
}

func (h *handler) patchUser(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDValue(r.PathValue("id"), "user_id")
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	req, ok := decode[patchUserRequest](h, w, r)
	if !ok {
		return
	}
	user, err := h.cfg.Admin.UpdateUser(r.Context(), adminID(r), id, service.UpdateUserInput{
		Login:    req.Login,
		Password: req.Password,
		Active:   req.Active,
		Admin:    req.IsAdmin,
	})
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	h.log.Info("admin action", "action", "update_user", "admin_id", adminID(r), "user_id", user.ID)
	httpx.JSON(w, r, http.StatusOK, user)
}

func (h *handler) listChannelsAdmin(w http.ResponseWriter, r *http.Request) {
	channels, err := h.cfg.Admin.ListChannels(r.Context())
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	if channels == nil {
		channels = []store.Channel{}
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"channels": channels, "count": len(channels)})
}

func (h *handler) createChannel(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[createChannelRequest](h, w, r)
	if !ok {
		return
	}
	channel, err := h.cfg.Admin.CreateChannel(r.Context(), service.CreateChannelInput{
		Number:     req.Number,
		Name:       req.Name,
		StreamPath: req.StreamPath,
		Active:     deref(req.Active, true),
	})
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	h.log.Info("admin action", "action", "create_channel", "admin_id", adminID(r), "channel_id", channel.ID)
	if err := httpx.JSON(w, r, http.StatusCreated, channel); err != nil {
		h.log.Error("could not write response", "error", err)
	}
}

func (h *handler) patchChannel(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDValue(r.PathValue("id"), "channel_id")
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	req, ok := decode[patchChannelRequest](h, w, r)
	if !ok {
		return
	}
	channel, err := h.cfg.Admin.UpdateChannel(r.Context(), id, service.UpdateChannelInput{
		Number:     req.Number,
		Name:       req.Name,
		StreamPath: req.StreamPath,
		Active:     req.Active,
	})
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	h.log.Info("admin action", "action", "update_channel", "admin_id", adminID(r), "channel_id", channel.ID)
	httpx.JSON(w, r, http.StatusOK, channel)
}

func (h *handler) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	userID, err := parseIDValue(r.PathValue("id"), "user_id")
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	subs, err := h.cfg.Admin.ListSubscriptions(r.Context(), userID)
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	if subs == nil {
		subs = []store.Subscription{}
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"user_id":       userID,
		"subscriptions": subs,
		"count":         len(subs),
	})
}

func (h *handler) grantSubscription(w http.ResponseWriter, r *http.Request) {
	userID, err := parseIDValue(r.PathValue("id"), "user_id")
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	channelID, err := parseIDValue(r.PathValue("channelId"), "channel_id")
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	req, ok := decode[grantRequest](h, w, r)
	if !ok {
		return
	}
	sub, err := h.cfg.Admin.Grant(r.Context(), userID, channelID, req.ExpiresAt)
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	h.log.Info("admin action", "action", "grant_subscription", "admin_id", adminID(r),
		"user_id", userID, "channel_id", channelID)
	httpx.JSON(w, r, http.StatusOK, sub)
}

func (h *handler) revokeSubscription(w http.ResponseWriter, r *http.Request) {
	userID, err := parseIDValue(r.PathValue("id"), "user_id")
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	channelID, err := parseIDValue(r.PathValue("channelId"), "channel_id")
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	removed, err := h.cfg.Admin.Revoke(r.Context(), userID, channelID)
	if err != nil {
		httpx.ErrorOf(w, r, h.log, err)
		return
	}
	if !removed {
		httpx.Error(w, r, http.StatusNotFound, "not_found", "the user has no subscription to this channel")
		return
	}
	h.log.Info("admin action", "action", "revoke_subscription", "admin_id", adminID(r),
		"user_id", userID, "channel_id", channelID)
	w.WriteHeader(http.StatusNoContent)
}

// decode reads and validates a JSON request body. Unknown fields are rejected:
// a typo in a field name ("is_active" written as "active") should fail loudly
// instead of silently doing the wrong thing.
func decode[T any](h *handler, w http.ResponseWriter, r *http.Request) (*T, bool) {
	var body T
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		switch {
		case errors.Is(err, io.EOF):
			httpx.Error(w, r, http.StatusBadRequest, "empty_body", "a request body is required")
		default:
			h.log.Warn("request body rejected", "error", err, "path", r.URL.Path)
			httpx.Error(w, r, http.StatusBadRequest, "invalid_json",
				"the request body is not valid JSON for this endpoint")
		}
		return nil, false
	}
	// Trailing content after a valid object means the client sent something
	// malformed; treat it as such rather than ignoring the rest.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		httpx.Error(w, r, http.StatusBadRequest, "invalid_json", "the request body must contain exactly one JSON object")
		return nil, false
	}
	return &body, true
}

func deref(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func adminID(r *http.Request) int64 {
	if principal, ok := auth.FromContext(r.Context()); ok {
		return principal.UserID
	}
	return 0
}
