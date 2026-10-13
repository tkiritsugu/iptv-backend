package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// accessQuery joins a channel with the single grant this user may have for it.
// The UNIQUE (user_id, channel_id) index guarantees at most one row per
// channel, so the join cannot duplicate channels.
const accessQuery = `
	SELECT c.id, c.number, c.name, c.stream_path, c.is_active,
	       s.id AS grant_id, s.expires_at
	  FROM channels c
	  LEFT JOIN subscriptions s ON (s.channel_id = c.id AND s.user_id = ?)
	 WHERE %s
	 ORDER BY c.number, c.id`

// AccessForUser returns every active channel with the access decision for one
// user. Admin accounts see all active channels: they operate the service and
// need to be able to preview a stream without owning a grant.
//
// Whether a channel is visible to the caller is decided by the caller (the API
// filters on ChannelAccess.Available); the store returns the full picture so
// that both "only what I can watch" and "everything with a flag" can be built
// from one query.
func (s *Store) AccessForUser(ctx context.Context, userID int64, admin bool) ([]ChannelAccess, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(accessQuery, "c.is_active = 1"), userID)
	if err != nil {
		return nil, fmt.Errorf("store: access list: %w", err)
	}
	defer rows.Close()

	now := s.now()
	var out []ChannelAccess
	for rows.Next() {
		ca, err := scanAccess(rows, now, admin)
		if err != nil {
			return nil, fmt.Errorf("store: access list scan: %w", err)
		}
		out = append(out, ca)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: access list rows: %w", err)
	}
	return out, nil
}

// Access decides whether one user may watch one channel.
//
// It distinguishes the three failure modes the API has to report differently:
// ErrNotFound (no such channel), ErrDisabled (channel switched off) and
// ErrNoSubscription (channel exists and is on, the user just may not watch it).
func (s *Store) Access(ctx context.Context, userID, channelID int64, admin bool) (ChannelAccess, error) {
	row := s.db.QueryRowContext(ctx,
		fmt.Sprintf(accessQuery, "c.id = ?"), userID, channelID)
	ca, err := scanAccess(row, s.now(), admin)
	if errors.Is(err, sql.ErrNoRows) {
		return ChannelAccess{}, ErrNotFound
	}
	if err != nil {
		return ChannelAccess{}, fmt.Errorf("store: access: %w", err)
	}
	switch {
	case !ca.Channel.Active:
		return ChannelAccess{}, ErrDisabled
	case !ca.Available:
		return ChannelAccess{}, ErrNoSubscription
	}
	return ca, nil
}

// NextExpiry returns the moment the earliest still valid time limited grant in
// the list stops granting access, or the zero time when there is none. The
// cache uses it as a TTL: an entry never outlives the fact it was built from,
// without anyone having to poll for expiry.
func NextExpiry(access []ChannelAccess) time.Time {
	var next time.Time
	for _, a := range access {
		if a.ExpiresAt == nil {
			continue
		}
		if next.IsZero() || a.ExpiresAt.Before(next) {
			next = *a.ExpiresAt
		}
	}
	return next
}

// scanAccess turns a joined row into a decision.
//
// grant_id, not expires_at, tells "no grant" apart from "grant without an
// expiry": both have a NULL expires_at, and conflating them would make every
// unlimited subscription look missing.
func scanAccess(row interface{ Scan(dest ...any) error }, now time.Time, admin bool) (ChannelAccess, error) {
	var (
		c       Channel
		active  int
		grant   sql.NullInt64
		expires sql.NullInt64
	)
	if err := row.Scan(&c.ID, &c.Number, &c.Name, &c.StreamPath, &active, &grant, &expires); err != nil {
		return ChannelAccess{}, err
	}
	c.Active = active == 1

	ca := ChannelAccess{Channel: c}
	if expires.Valid {
		t := time.Unix(expires.Int64, 0)
		ca.ExpiresAt = &t
	}
	switch {
	case admin:
		// The admin role is not a subscription: no grant, no expiry.
		ca.Available, ca.ExpiresAt = c.Active, nil
	case !c.Active, !grant.Valid:
		ca.Available = false
	case expires.Valid:
		// A grant with an end moment: it stops working on its own.
		ca.Available = expires.Int64 > now.Unix()
	default:
		ca.Available = true
	}
	return ca, nil
}
