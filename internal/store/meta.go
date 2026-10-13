package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Data groups whose writes must invalidate cached reads.
const (
	// VersionUsers covers the users table: logins, passwords, active flag,
	// admin flag.
	VersionUsers = "users"
	// VersionCatalog covers channels and subscriptions, i.e. everything that
	// decides which channels a user may see and watch.
	VersionCatalog = "catalog"
)

// write runs fn inside a transaction and bumps the given version counters in
// the same transaction. Bumping inside the transaction is what makes cache
// invalidation reliable: a caller can never observe the new data with the old
// counter, because both become visible at commit.
//
// Domain errors returned by fn are passed through unwrapped so that the caller
// can still match them with errors.Is.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error, versions ...string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once the tx is committed

	if err := fn(tx); err != nil {
		return err
	}
	for _, key := range versions {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO data_version (key, value) VALUES (?, 2)
			 ON CONFLICT (key) DO UPDATE SET value = value + 1`, key); err != nil {
			return fmt.Errorf("store: bump version %q: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Version returns the current counter of a data group. It is the only database
// read the cache layer performs on the hot path, and callers poll it at most
// once per second per instance.
func (s *Store) Version(ctx context.Context, key string) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, `SELECT value FROM data_version WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		// A group nobody has written to yet; 1 matches the schema baseline.
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: version %q: %w", key, err)
	}
	return v, nil
}
