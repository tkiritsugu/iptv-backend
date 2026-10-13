// Package store is the SQLite persistence layer.
//
// All queries are plain parameterised SQL through database/sql — there is no
// ORM. The package knows about HTTP nothing: it returns typed domain errors
// (ErrNotFound, ErrConflict, ErrDisabled, ErrNoSubscription) that the service
// layer translates into status codes.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver, no cgo
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store gives access to the database.
type Store struct {
	db       *sql.DB
	log      *slog.Logger
	now      func() time.Time
	maxConns int
}

// Option tweaks the store.
type Option func(*Store)

// WithClock replaces the time source used to decide whether a subscription is
// still valid, which makes expiry testable without waiting.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// WithMaxConns sets the connection pool size.
func WithMaxConns(n int) Option {
	return func(s *Store) {
		if n > 0 {
			s.maxConns = n
		}
	}
}

// DSN builds a connection string with the pragmas the service relies on.
//
//	busy_timeout  — writers wait instead of failing with "database is locked"
//	                (journal_mode=WAL lets readers never wait at all)
//	foreign_keys  — off by default in SQLite, on here so the cascade rules in
//	                the schema actually apply
func DSN(path string) string {
	pragma := url.Values{}
	pragma.Add("_pragma", "busy_timeout(5000)")
	pragma.Add("_pragma", "foreign_keys(1)")
	if path == ":memory:" {
		// A unique name with a shared cache: without it every connection of the
		// pool would get its own private in-memory database. The journal cannot
		// be WAL for a memory database, so only the two pragmas above.
		name := "iptv-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		return "file:" + name + "?mode=memory&cache=shared&" + pragma.Encode()
	}
	pragma.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + path + "?" + pragma.Encode()
}

// Open connects to the database, applies pending migrations and returns a
// ready store. The caller owns Close.
func Open(ctx context.Context, dsn string, log *slog.Logger, opts ...Option) (*Store, error) {
	s := &Store{log: log, now: time.Now, maxConns: 16}
	for _, opt := range opts {
		opt(s)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// SQLite serialises writers but serves readers concurrently in WAL mode, so
	// a pool of readers is what actually helps. Idle connections are kept as
	// long as the pool allows: for a shared in-memory database closing the last
	// connection would destroy the data.
	db.SetMaxOpenConns(s.maxConns)
	db.SetMaxIdleConns(s.maxConns)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	s.db = db

	if mode, err := s.pragmaJournalMode(ctx); err != nil {
		// Not fatal: WAL is an optimisation, correctness does not depend on it.
		s.log.Warn("could not enable WAL journal mode", "mode", mode, "error", err)
	} else {
		s.log.Info("database ready", "journal_mode", mode)
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) pragmaJournalMode(ctx context.Context) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode)
	return mode, err
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Ping reports whether the database is reachable; used by the readiness probe.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// DB exposes the underlying handle. Only used by tests that need to inspect
// state directly.
func (s *Store) DB() *sql.DB { return s.db }

// migrate applies embedded migrations that have not run yet, each in its own
// transaction, and records progress in PRAGMA user_version. The schema is
// therefore created automatically on first start, which is what the task asks
// for, while still being a real migration path for later changes.
func (s *Store) migrate(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("store: read migrations: %w", err)
	}
	for i, entry := range entries {
		want := i + 1
		var n int
		if scanned, err := fmt.Sscanf(entry.Name(), "%04d_", &n); err != nil || scanned != 1 || n != want {
			return fmt.Errorf("store: migration %q must be named %04d_name.sql", entry.Name(), want)
		}
		if want <= current {
			continue
		}
		body, err := fs.ReadFile(migrations, "migrations/"+entry.Name())
		if err != nil {
			return fmt.Errorf("store: read migration %q: %w", entry.Name(), err)
		}
		if err := s.applyMigration(ctx, entry.Name(), want, string(body)); err != nil {
			return err
		}
		s.log.Info("applied migration", "name", entry.Name(), "version", want)
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, name string, version int, body string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %q: %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck // ignored after commit

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("store: migration %q: %w", name, err)
	}
	// PRAGMA values cannot be bound as parameters; version is an int computed
	// from the file name, so there is no injection risk.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("store: migration %q version: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %q: %w", name, err)
	}
	return nil
}

// Domain errors. They carry no database details on purpose: the HTTP layer can
// render them verbatim without leaking SQL text.
var (
	// ErrNotFound is returned when a row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned when a uniqueness rule blocks a write.
	ErrConflict = errors.New("already exists")
	// ErrDisabled reports an object that exists but is switched off.
	ErrDisabled = errors.New("disabled")
	// ErrNoSubscription reports a channel the user may not watch.
	ErrNoSubscription = errors.New("no subscription")
	// ErrInUse reports a referenced object, e.g. a subscription pointing at an
	// unknown channel.
	ErrInUse = errors.New("referenced object does not exist")
)

// isUniqueConstraint reports whether err is a SQLite UNIQUE constraint
// violation. The driver error type is matched through an interface so that the
// rest of the package stays driver agnostic.
func isUniqueConstraint(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	const sqliteConstraint = 19
	const sqliteConstraintUnique = 2067
	const sqliteConstraintPrimaryKey = 1555
	switch coded.Code() {
	case sqliteConstraint, sqliteConstraintUnique, sqliteConstraintPrimaryKey:
		return true
	default:
		return strings.Contains(err.Error(), "UNIQUE constraint failed")
	}
}
