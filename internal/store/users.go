package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const userColumns = `id, login, password_hash, is_active, is_admin, created_at, updated_at`

func scanUser(row interface{ Scan(dest ...any) error }) (User, error) {
	var (
		u                User
		active, admin    int
		created, updated int64
	)
	err := row.Scan(&u.ID, &u.Login, &u.PasswordHash, &active, &admin, &created, &updated)
	if err != nil {
		return User{}, err
	}
	u.Active, u.Admin = active == 1, admin == 1
	u.CreatedAt, u.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return u, nil
}

// CreateUser inserts a user and fills in the generated id and timestamps.
func (s *Store) CreateUser(ctx context.Context, in NewUser) (User, error) {
	now := s.now()
	var out User
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO users (login, password_hash, is_active, is_admin, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			in.Login, in.PasswordHash, b2i(in.Active), b2i(in.Admin), now.Unix(), now.Unix())
		if err != nil {
			if isUniqueConstraint(err) {
				return ErrConflict
			}
			return fmt.Errorf("store: create user: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create user id: %w", err)
		}
		out = User{
			ID:           id,
			Login:        in.Login,
			PasswordHash: in.PasswordHash,
			Active:       in.Active,
			Admin:        in.Admin,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		return nil
	}, VersionUsers)
	if err != nil {
		return User{}, err
	}
	return out, nil
}

// UserPatch lists the fields to change; nil means "leave as is".
type UserPatch struct {
	Login        *string
	PasswordHash *string
	Active       *bool
	Admin        *bool
}

// Empty reports whether the patch would change anything.
func (p UserPatch) Empty() bool {
	return p.Login == nil && p.PasswordHash == nil && p.Active == nil && p.Admin == nil
}

// UpdateUser applies a patch and returns the stored row afterwards.
func (s *Store) UpdateUser(ctx context.Context, id int64, patch UserPatch) (User, error) {
	var (
		set  []string
		args []any
	)
	if patch.Login != nil {
		set = append(set, "login = ?")
		args = append(args, *patch.Login)
	}
	if patch.PasswordHash != nil {
		set = append(set, "password_hash = ?")
		args = append(args, *patch.PasswordHash)
	}
	if patch.Active != nil {
		set = append(set, "is_active = ?")
		args = append(args, b2i(*patch.Active))
	}
	if patch.Admin != nil {
		set = append(set, "is_admin = ?")
		args = append(args, b2i(*patch.Admin))
	}
	if len(set) == 0 {
		return s.UserByID(ctx, id)
	}
	args = append(args, s.now().Unix(), id)
	query := `UPDATE users SET ` + strings.Join(set, ", ") + `, updated_at = ? WHERE id = ?`

	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			if isUniqueConstraint(err) {
				return ErrConflict
			}
			return fmt.Errorf("store: update user: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrNotFound
		}
		return nil
	}, VersionUsers)
	if err != nil {
		return User{}, err
	}
	return s.UserByID(ctx, id)
}

// UserByID loads a user. ErrNotFound when absent.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: get user: %w", err)
	}
	return u, nil
}

// UserByLogin loads a user by login. Lookups are case sensitive: logins are
// stored exactly as created, which keeps authentication and the UNIQUE index in
// agreement.
func (s *Store) UserByLogin(ctx context.Context, login string) (User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE login = ?`, login)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: get user by login: %w", err)
	}
	return u, nil
}

// ListUsers returns every user ordered by login.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY login, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list users scan: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list users rows: %w", err)
	}
	return users, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
