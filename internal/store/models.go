package store

import "time"

// User is an account. PasswordHash holds the derived key only; the plaintext
// password never reaches this package's data types, so it cannot be logged or
// serialised by accident.
type User struct {
	ID           int64     `json:"id"`
	Login        string    `json:"login"`
	PasswordHash string    `json:"-"`
	Active       bool      `json:"is_active"`
	Admin        bool      `json:"is_admin"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Channel is a TV channel. StreamPath is the path part of the playback URL;
// the origin comes from configuration so a CDN move does not rewrite rows.
type Channel struct {
	ID         int64     `json:"id"`
	Number     int       `json:"number"`
	Name       string    `json:"name"`
	StreamPath string    `json:"stream_path"`
	Active     bool      `json:"is_active"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Subscription grants a user access to a channel. A nil ExpiresAt means the
// grant is unlimited and is only switched off by an administrator.
type Subscription struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"user_id"`
	ChannelID  int64      `json:"channel_id"`
	ChannelNum int        `json:"channel_number,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Active reports whether the grant is valid at the given moment. Keeping the
// rule in one place means the list, the playback link and the admin view cannot
// disagree about what "expired" means.
func (s *Subscription) Active(now time.Time) bool {
	return s.ExpiresAt == nil || s.ExpiresAt.After(now)
}

// ChannelAccess is a channel together with the access decision for one user.
type ChannelAccess struct {
	Channel   Channel
	Available bool
	ExpiresAt *time.Time
}

// NewUser is the input for creating a user.
type NewUser struct {
	Login        string
	PasswordHash string
	Active       bool
	Admin        bool
}

// NewChannel is the input for creating a channel.
type NewChannel struct {
	Number     int
	Name       string
	StreamPath string
	Active     bool
}
