package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	"iptv/internal/httpx"
	"iptv/internal/password"
	"iptv/internal/store"
)

// Validation limits, shared by the admin API and the panel forms.
const (
	minLoginLen = 3
	maxLoginLen = 32
	minNumber   = 1
	maxNumber   = 99_999
	maxNameLen  = 80
	maxPathLen  = 200
)

// Stable errors of the admin side.
var (
	// ErrSelfLockout stops an administrator from switching off their own
	// account: with no other admin in the system that is how a service becomes
	// unmanageable.
	ErrSelfLockout = httpx.NewError(409, "self_lockout",
		"you cannot deactivate your own account or remove your own admin role")
	// ErrUnknownReference is returned when a grant points at a user or channel
	// that does not exist.
	ErrUnknownReference = httpx.NewError(404, "reference_not_found",
		"the referenced user or channel does not exist")
)

// InvalidArgument builds a 400 with a specific code so a client can react to
// the failed field rather than parse prose.
func InvalidArgument(code, message string) error {
	return httpx.NewError(http.StatusBadRequest, code, message)
}

// AdminStore is the part of the store the admin service uses.
type AdminStore interface {
	ListUsers(ctx context.Context) ([]store.User, error)
	UserByID(ctx context.Context, id int64) (store.User, error)
	CreateUser(ctx context.Context, in store.NewUser) (store.User, error)
	UpdateUser(ctx context.Context, id int64, patch store.UserPatch) (store.User, error)

	ListChannels(ctx context.Context) ([]store.Channel, error)
	ChannelByID(ctx context.Context, id int64) (store.Channel, error)
	CreateChannel(ctx context.Context, in store.NewChannel) (store.Channel, error)
	UpdateChannel(ctx context.Context, id int64, patch store.ChannelPatch) (store.Channel, error)

	GrantSubscription(ctx context.Context, userID, channelID int64, expiresAt *time.Time) (store.Subscription, error)
	RevokeSubscription(ctx context.Context, userID, channelID int64) (bool, error)
	ListSubscriptions(ctx context.Context, userID int64) ([]store.Subscription, error)
}

// Admin applies configuration changes. Every write that affects what a user may
// see invalidates the local read cache; the version counter in the database
// takes care of the other instances.
type Admin struct {
	store AdminStore
	// invalidators are the read caches that configuration changes touch. The
	// instance that performs a write clears them immediately; other instances
	// converge when they next read the version counter.
	invalidators []Invalidator
	hasher       password.Hasher
	log          *slog.Logger
	now          func() time.Time
}

// Invalidator clears cached reads. Implemented by *Channels.
type Invalidator interface {
	InvalidateAll()
}

// NewAdmin builds the admin service. The hasher decides the cost of every new
// password; tests pass a cheap one. invalidators are the caches to clear after a
// successful write — typically the read cache and the credential cache.
func NewAdmin(st AdminStore, hasher password.Hasher, log *slog.Logger, invalidators ...Invalidator) *Admin {
	return &Admin{store: st, hasher: hasher, log: log, now: time.Now, invalidators: invalidators}
}

func (a *Admin) invalidate() {
	for _, inv := range a.invalidators {
		if inv != nil {
			inv.InvalidateAll()
		}
	}
}

// WithNow replaces the time source of the admin service, for tests.
func (a *Admin) WithNow(now func() time.Time) { a.now = now }

// CreateUserInput is a validated create request.
type CreateUserInput struct {
	Login    string
	Password string
	Active   bool
	Admin    bool
}

// CreateUser validates and inserts a user.
func (a *Admin) CreateUser(ctx context.Context, in CreateUserInput) (store.User, error) {
	login, err := ValidateLogin(in.Login)
	if err != nil {
		return store.User{}, err
	}
	if err := password.Validate(in.Password); err != nil {
		return store.User{}, InvalidArgument("invalid_password", err.Error())
	}
	hash, err := a.hasher.Hash(in.Password)
	if err != nil {
		a.log.Error("could not hash password", "error", err)
		return store.User{}, httpx.NewError(500, "internal_error", "the request could not be completed")
	}
	user, err := a.store.CreateUser(ctx, store.NewUser{
		Login:        login,
		PasswordHash: hash,
		Active:       in.Active,
		Admin:        in.Admin,
	})
	if err != nil {
		return store.User{}, mapStoreError(err)
	}
	// A new login affects the credential cache, not what anybody may watch.
	a.invalidate()
	a.log.Info("admin: user created", "user_id", user.ID, "login", user.Login,
		"is_admin", user.Admin, "is_active", user.Active)
	return user, nil
}

// UpdateUserInput carries the optional fields of a PATCH request.
type UpdateUserInput struct {
	Login    *string
	Password *string
	Active   *bool
	Admin    *bool
}

// UpdateUser applies a patch, refusing to let an administrator lock themselves
// out.
func (a *Admin) UpdateUser(ctx context.Context, actorID, userID int64, in UpdateUserInput) (store.User, error) {
	demoteSelf := userID == actorID &&
		((in.Admin != nil && !*in.Admin) || (in.Active != nil && !*in.Active))
	if demoteSelf {
		return store.User{}, ErrSelfLockout
	}
	patch := store.UserPatch{}
	if in.Login != nil {
		login, err := ValidateLogin(*in.Login)
		if err != nil {
			return store.User{}, err
		}
		patch.Login = &login
	}
	if in.Password != nil {
		if err := password.Validate(*in.Password); err != nil {
			return store.User{}, InvalidArgument("invalid_password", err.Error())
		}
		hash, err := a.hasher.Hash(*in.Password)
		if err != nil {
			return store.User{}, httpx.NewError(500, "internal_error", "the request could not be completed")
		}
		patch.PasswordHash = &hash
	}
	patch.Active = in.Active
	patch.Admin = in.Admin

	user, err := a.store.UpdateUser(ctx, userID, patch)
	if err != nil {
		return store.User{}, mapStoreError(err)
	}
	// Disabling an account or changing a password must not wait for the cached
	// credentials to expire: drop them here, and let the version counter do the
	// same for the other instances.
	a.invalidate()
	a.log.Info("admin: user updated", "user_id", user.ID, "login", user.Login,
		"is_active", user.Active, "is_admin", user.Admin,
		"password_changed", patch.PasswordHash != nil)
	return user, nil
}

// ListUsers returns every user.
func (a *Admin) ListUsers(ctx context.Context) ([]store.User, error) {
	users, err := a.store.ListUsers(ctx)
	return users, mapStoreError(err)
}

// CreateChannelInput is a validated create request.
type CreateChannelInput struct {
	Number     int
	Name       string
	StreamPath string
	Active     bool
}

// CreateChannel validates and inserts a channel.
func (a *Admin) CreateChannel(ctx context.Context, in CreateChannelInput) (store.Channel, error) {
	name, err := ValidateChannelName(in.Name)
	if err != nil {
		return store.Channel{}, err
	}
	path, err := ValidateStreamPath(in.StreamPath)
	if err != nil {
		return store.Channel{}, err
	}
	if err := ValidateNumber(in.Number); err != nil {
		return store.Channel{}, err
	}
	channel, err := a.store.CreateChannel(ctx, store.NewChannel{
		Number:     in.Number,
		Name:       name,
		StreamPath: path,
		Active:     in.Active,
	})
	if err != nil {
		return store.Channel{}, mapStoreError(err)
	}
	a.invalidate()
	a.log.Info("admin: channel created", "channel_id", channel.ID, "number", channel.Number,
		"is_active", channel.Active)
	return channel, nil
}

// UpdateChannelInput carries the optional fields of a PATCH request.
type UpdateChannelInput struct {
	Number     *int
	Name       *string
	StreamPath *string
	Active     *bool
}

// UpdateChannel applies a patch.
func (a *Admin) UpdateChannel(ctx context.Context, id int64, in UpdateChannelInput) (store.Channel, error) {
	patch := store.ChannelPatch{}
	if in.Number != nil {
		if err := ValidateNumber(*in.Number); err != nil {
			return store.Channel{}, err
		}
		patch.Number = in.Number
	}
	if in.Name != nil {
		name, err := ValidateChannelName(*in.Name)
		if err != nil {
			return store.Channel{}, err
		}
		patch.Name = &name
	}
	if in.StreamPath != nil {
		path, err := ValidateStreamPath(*in.StreamPath)
		if err != nil {
			return store.Channel{}, err
		}
		patch.StreamPath = &path
	}
	patch.Active = in.Active

	channel, err := a.store.UpdateChannel(ctx, id, patch)
	if err != nil {
		return store.Channel{}, mapStoreError(err)
	}
	a.invalidate()
	a.log.Info("admin: channel updated", "channel_id", channel.ID, "number", channel.Number,
		"is_active", channel.Active)
	return channel, nil
}

// ListChannels returns every channel, active or not.
func (a *Admin) ListChannels(ctx context.Context) ([]store.Channel, error) {
	channels, err := a.store.ListChannels(ctx)
	return channels, mapStoreError(err)
}

// Grant gives a user access to a channel. A nil expiresAt means unlimited.
func (a *Admin) Grant(ctx context.Context, userID, channelID int64, expiresAt *time.Time) (store.Subscription, error) {
	if expiresAt != nil && !expiresAt.After(a.now()) {
		// Accepted on purpose: setting a moment in the past is the natural way
		// for an operator to end a subscription right now.
		a.log.Info("admin: granting an already expired subscription",
			"user_id", userID, "channel_id", channelID, "expires_at", expiresAt)
	}
	sub, err := a.store.GrantSubscription(ctx, userID, channelID, expiresAt)
	if err != nil {
		return store.Subscription{}, mapStoreError(err)
	}
	a.invalidate()
	a.log.Info("admin: subscription granted", "user_id", userID, "channel_id", channelID,
		"expires_at", formatOptional(expiresAt))
	return sub, nil
}

// Revoke removes a grant and reports whether there was one.
func (a *Admin) Revoke(ctx context.Context, userID, channelID int64) (bool, error) {
	removed, err := a.store.RevokeSubscription(ctx, userID, channelID)
	if err != nil {
		return false, mapStoreError(err)
	}
	if removed {
		a.invalidate()
		a.log.Info("admin: subscription revoked", "user_id", userID, "channel_id", channelID)
	}
	return removed, nil
}

// ListSubscriptions returns the grants of one user.
func (a *Admin) ListSubscriptions(ctx context.Context, userID int64) ([]store.Subscription, error) {
	if _, err := a.store.UserByID(ctx, userID); err != nil {
		return nil, mapStoreError(err)
	}
	subs, err := a.store.ListSubscriptions(ctx, userID)
	return subs, mapStoreError(err)
}

// ValidateLogin normalises and checks a login name.
func ValidateLogin(raw string) (string, error) {
	login := strings.TrimSpace(raw)
	if len(login) < minLoginLen || len(login) > maxLoginLen {
		return "", InvalidArgument("invalid_login",
			fmt.Sprintf("login must be %d to %d characters", minLoginLen, maxLoginLen))
	}
	for i, r := range login {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
		case r == '.' || r == '_' || r == '-':
			if i == 0 {
				return "", InvalidArgument("invalid_login", "login must start with a letter or a digit")
			}
		default:
			return "", InvalidArgument("invalid_login",
				"login may contain letters, digits, dot, underscore and hyphen")
		}
	}
	return login, nil
}

// ValidateChannelName normalises and checks a channel name.
func ValidateChannelName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if n := len([]rune(name)); n == 0 {
		return "", InvalidArgument("invalid_name", "name must not be empty")
	} else if n > maxNameLen {
		return "", InvalidArgument("invalid_name", fmt.Sprintf("name must be at most %d characters", maxNameLen))
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", InvalidArgument("invalid_name", "name must not contain control characters")
		}
	}
	return name, nil
}

// ValidateNumber checks a channel position.
func ValidateNumber(n int) error {
	if n < minNumber || n > maxNumber {
		return InvalidArgument("invalid_number",
			fmt.Sprintf("number must be between %d and %d", minNumber, maxNumber))
	}
	return nil
}

// ValidateStreamPath checks the path part of a playback URL.
//
// A path is stored, concatenated with a configured origin and handed to clients,
// so it is checked on the way in rather than sanitised on the way out: no query,
// no fragment, no traversal, no spaces.
func ValidateStreamPath(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	switch {
	case len(path) > maxPathLen:
		return "", InvalidArgument("invalid_stream_path",
			fmt.Sprintf("stream_path must be at most %d characters", maxPathLen))
	case !strings.HasPrefix(path, "/"):
		return "", InvalidArgument("invalid_stream_path", `stream_path must start with "/"`)
	case strings.ContainsAny(path, "?# \t\n\\"):
		return "", InvalidArgument("invalid_stream_path",
			"stream_path must not contain a query, a fragment, spaces or backslashes")
	case strings.Contains(path, ".."):
		return "", InvalidArgument("invalid_stream_path", "stream_path must not contain \"..\"")
	}
	for _, r := range path {
		if unicode.IsControl(r) {
			return "", InvalidArgument("invalid_stream_path",
				"stream_path must not contain control characters")
		}
	}
	return path, nil
}

// mapStoreError translates storage facts into API errors. Nothing below this
// function reaches the client: a database error text never becomes a response.
func mapStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return httpx.NewError(404, "not_found", "the requested object does not exist")
	case errors.Is(err, store.ErrConflict):
		return httpx.NewError(409, "already_exists", "that login or channel number is already taken")
	case errors.Is(err, store.ErrInUse):
		return ErrUnknownReference
	case errors.Is(err, store.ErrNoSubscription):
		return ErrSubscriptionRequired
	case errors.Is(err, store.ErrDisabled):
		return ErrChannelDisabled
	default:
		// An unexpected storage failure travels up unchanged: the handler logs
		// it and answers with a generic error, so the detail never reaches the
		// client.
		return err
	}
}

func formatOptional(t *time.Time) string {
	if t == nil {
		return "unlimited"
	}
	return t.UTC().Format(time.RFC3339)
}
