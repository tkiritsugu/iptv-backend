-- Initial schema.
--
-- Timestamps are unix seconds (INTEGER), not DATETIME text. SQLite has no
-- native timestamp type; storing an integer keeps comparisons exact, immune to
-- time zone and formatting differences, and index friendly. The API layer
-- renders RFC 3339 strings.

CREATE TABLE users (
	id            INTEGER PRIMARY KEY,
	login         TEXT    NOT NULL UNIQUE,
	password_hash TEXT    NOT NULL,
	is_active     INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0, 1)),
	is_admin      INTEGER NOT NULL DEFAULT 0 CHECK (is_admin IN (0, 1)),
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);

CREATE TABLE channels (
	id          INTEGER PRIMARY KEY,
	number      INTEGER NOT NULL UNIQUE,
	name        TEXT    NOT NULL,
	stream_path TEXT    NOT NULL,
	is_active   INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0, 1)),
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);

-- One row per (user, channel) grant. expires_at NULL means "unlimited"; a value
-- in the past means "expired", and expired grants stop working on their own
-- because access is decided by comparing expires_at with the current time on
-- every read. No cron job, no cleanup task, no window where an expired
-- subscription still grants access.
CREATE TABLE subscriptions (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	channel_id INTEGER NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
	expires_at INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE (user_id, channel_id)
);

CREATE INDEX idx_subscriptions_user ON subscriptions (user_id);

-- Monotonic counters used to invalidate in-process caches. Any write to users
-- bumps "users", any write to channels or subscriptions bumps "catalog". Each
-- instance polls the counters at most once per second, which bounds how long a
-- change takes to become visible on the other instances.
CREATE TABLE data_version (
	key   TEXT    PRIMARY KEY,
	value INTEGER NOT NULL
);

INSERT INTO data_version (key, value) VALUES ('users', 1), ('catalog', 1);
