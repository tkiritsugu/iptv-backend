package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const channelColumns = `id, number, name, stream_path, is_active, created_at, updated_at`

func scanChannel(row interface{ Scan(dest ...any) error }) (Channel, error) {
	var (
		c                Channel
		active           int
		created, updated int64
	)
	err := row.Scan(&c.ID, &c.Number, &c.Name, &c.StreamPath, &active, &created, &updated)
	if err != nil {
		return Channel{}, err
	}
	c.Active = active == 1
	c.CreatedAt, c.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return c, nil
}

// CreateChannel inserts a channel.
func (s *Store) CreateChannel(ctx context.Context, in NewChannel) (Channel, error) {
	now := s.now()
	var out Channel
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO channels (number, name, stream_path, is_active, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			in.Number, in.Name, in.StreamPath, b2i(in.Active), now.Unix(), now.Unix())
		if err != nil {
			if isUniqueConstraint(err) {
				return fmt.Errorf("%w: channel number %d is taken", ErrConflict, in.Number)
			}
			return fmt.Errorf("store: create channel: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create channel id: %w", err)
		}
		out = Channel{
			ID:         id,
			Number:     in.Number,
			Name:       in.Name,
			StreamPath: in.StreamPath,
			Active:     in.Active,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		return nil
	}, VersionCatalog)
	if err != nil {
		return Channel{}, err
	}
	return out, nil
}

// ChannelPatch lists the fields to change; nil means "leave as is".
type ChannelPatch struct {
	Number     *int
	Name       *string
	StreamPath *string
	Active     *bool
}

// UpdateChannel applies a patch and returns the stored row afterwards.
func (s *Store) UpdateChannel(ctx context.Context, id int64, patch ChannelPatch) (Channel, error) {
	var set []string
	var args []any
	if patch.Number != nil {
		set = append(set, "number = ?")
		args = append(args, *patch.Number)
	}
	if patch.Name != nil {
		set = append(set, "name = ?")
		args = append(args, *patch.Name)
	}
	if patch.StreamPath != nil {
		set = append(set, "stream_path = ?")
		args = append(args, *patch.StreamPath)
	}
	if patch.Active != nil {
		set = append(set, "is_active = ?")
		args = append(args, b2i(*patch.Active))
	}
	if len(set) == 0 {
		return s.ChannelByID(ctx, id)
	}
	args = append(args, s.now().Unix(), id)
	query := `UPDATE channels SET ` + strings.Join(set, ", ") + `, updated_at = ? WHERE id = ?`

	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			if isUniqueConstraint(err) {
				return fmt.Errorf("%w: channel number is taken", ErrConflict)
			}
			return fmt.Errorf("store: update channel: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrNotFound
		}
		return nil
	}, VersionCatalog)
	if err != nil {
		return Channel{}, err
	}
	return s.ChannelByID(ctx, id)
}

// ChannelByID loads a channel, active or not.
func (s *Store) ChannelByID(ctx context.Context, id int64) (Channel, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+channelColumns+` FROM channels WHERE id = ?`, id)
	c, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNotFound
	}
	if err != nil {
		return Channel{}, fmt.Errorf("store: get channel: %w", err)
	}
	return c, nil
}

// ListChannels returns every channel ordered by position.
func (s *Store) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelColumns+` FROM channels ORDER BY number, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list channels: %w", err)
	}
	defer rows.Close()

	var channels []Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list channels scan: %w", err)
		}
		channels = append(channels, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list channels rows: %w", err)
	}
	return channels, nil
}
