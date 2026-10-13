package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func scanSubscription(row interface{ Scan(dest ...any) error }) (Subscription, error) {
	var (
		id, userID, channelID int64
		expires               sql.NullInt64
		created, updated      int64
	)
	if err := row.Scan(&id, &userID, &channelID, &expires, &created, &updated); err != nil {
		return Subscription{}, err
	}
	return newSubscription(id, userID, channelID, expires, created, updated), nil
}

func newSubscription(id, userID, channelID int64, expires sql.NullInt64, created, updated int64) Subscription {
	sub := Subscription{
		ID:        id,
		UserID:    userID,
		ChannelID: channelID,
		CreatedAt: time.Unix(created, 0),
		UpdatedAt: time.Unix(updated, 0),
	}
	if expires.Valid {
		t := time.Unix(expires.Int64, 0)
		sub.ExpiresAt = &t
	}
	return sub
}

// GrantSubscription creates or replaces a grant between a user and a channel.
// A nil expiresAt means unlimited; a value in the past is accepted and simply
// means the grant is already expired.
func (s *Store) GrantSubscription(ctx context.Context, userID, channelID int64, expiresAt *time.Time) (Subscription, error) {
	now := s.now()
	var out Subscription
	err := s.write(ctx, func(tx *sql.Tx) error {
		// Foreign keys are enforced, but checking explicitly gives a precise
		// error instead of a constraint violation for the caller to interpret.
		if err := exists(ctx, tx, "users", userID); err != nil {
			return err
		}
		if err := exists(ctx, tx, "channels", channelID); err != nil {
			return err
		}
		var expiresArg any
		if expiresAt != nil {
			expiresArg = expiresAt.Unix()
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO subscriptions (user_id, channel_id, expires_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (user_id, channel_id)
			 DO UPDATE SET expires_at = excluded.expires_at, updated_at = excluded.updated_at`,
			userID, channelID, expiresArg, now.Unix(), now.Unix())
		if err != nil {
			return fmt.Errorf("store: grant subscription: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: grant subscription id: %w", err)
		}
		out = Subscription{
			ID:        id,
			UserID:    userID,
			ChannelID: channelID,
			ExpiresAt: expiresAt,
			CreatedAt: now,
			UpdatedAt: now,
		}
		return nil
	}, VersionCatalog)
	if err != nil {
		return Subscription{}, err
	}
	return out, nil
}

// RevokeSubscription removes a grant. Missing grant or unknown user: false.
func (s *Store) RevokeSubscription(ctx context.Context, userID, channelID int64) (bool, error) {
	var removed int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM subscriptions WHERE user_id = ? AND channel_id = ?`, userID, channelID)
		if err != nil {
			return fmt.Errorf("store: revoke subscription: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: revoke subscription rows: %w", err)
		}
		removed = n
		return nil
	}, VersionCatalog)
	if err != nil {
		return false, err
	}
	return removed > 0, nil
}

// ListSubscriptions returns the grants of a user, with the channel number for
// display. Grants of inactive channels are included: the admin needs to see
// them.
func (s *Store) ListSubscriptions(ctx context.Context, userID int64) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.user_id, s.channel_id, s.expires_at, s.created_at, s.updated_at, c.number
		   FROM subscriptions s JOIN channels c ON c.id = s.channel_id
		  WHERE s.user_id = ?
		  ORDER BY c.number, s.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list subscriptions: %w", err)
	}
	defer rows.Close()

	var subs []Subscription
	for rows.Next() {
		var (
			id, userID, channelID int64
			expires               sql.NullInt64
			created, updated      int64
			number                int
		)
		if err := rows.Scan(&id, &userID, &channelID, &expires, &created, &updated, &number); err != nil {
			return nil, fmt.Errorf("store: list subscriptions scan: %w", err)
		}
		sub := newSubscription(id, userID, channelID, expires, created, updated)
		sub.ChannelNum = number
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list subscriptions rows: %w", err)
	}
	return subs, nil
}

// exists reports ErrInUse when a row is missing.
func exists(ctx context.Context, tx *sql.Tx, table string, id int64) error {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM `+table+` WHERE id = ?`, id).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s %d", ErrInUse, table, id)
	case err != nil:
		return fmt.Errorf("store: check %s %d: %w", table, id, err)
	}
	return nil
}
