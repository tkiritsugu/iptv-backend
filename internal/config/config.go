// Package config builds the service configuration.
//
// Precedence, lowest to highest: built-in defaults, environment variables,
// command line flags. Every option therefore has both an -addr flag and an
// IPTV_ADDR variable; the defaults are chosen so that the service starts with
// no configuration at all.
package config

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable knob of the service.
type Config struct {
	// HTTP
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	// Per-request budget for application work (database access). Connection
	// handling is covered by the server timeouts above.
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	// MaxBodyBytes limits request bodies (admin writes).
	MaxBodyBytes int64

	// Database
	DatabasePath string
	// DatabaseMaxConns caps the connection pool. SQLite serves many readers
	// concurrently in WAL mode but serialises writers, so a moderate pool is
	// enough and keeps "database is locked" retries rare.
	DatabaseMaxConns int

	// Seed populates demo data when the database is empty.
	Seed bool

	// Logging
	LogLevel  slog.Level
	LogFormat string // "json" or "text"

	// Playback links
	StreamBaseURL string
	StreamSigning bool
	StreamKey     string
	StreamTTL     time.Duration

	// Caching
	CacheTTL        time.Duration
	CacheVersionTTL time.Duration
	CacheStaleTTL   time.Duration
	CacheMaxEntries int
	AuthCacheTTL    time.Duration

	// Brute force protection on failed logins.
	LoginLimitPerIP  int
	LoginLimitWindow time.Duration
}

// Default returns the configuration used when nothing is configured.
//
// The values are safe for local development: an SQLite file under ./data and a
// listener on every interface on port 8080.
func Default() *Config {
	return &Config{
		Addr:              ":8080",
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       90 * time.Second,
		RequestTimeout:    5 * time.Second,
		ShutdownTimeout:   15 * time.Second,
		MaxBodyBytes:      64 << 10,

		DatabasePath:     "./data/iptv.db",
		DatabaseMaxConns: 16,
		Seed:             true,

		LogLevel:  slog.LevelInfo,
		LogFormat: "json",

		StreamBaseURL: "http://stream.example.com",
		StreamSigning: true,
		StreamKey:     "",
		StreamTTL:     15 * time.Minute,

		CacheTTL:        5 * time.Minute,
		CacheVersionTTL: time.Second,
		CacheStaleTTL:   10 * time.Minute,
		CacheMaxEntries: 8192,
		AuthCacheTTL:    5 * time.Second,

		LoginLimitPerIP:  10,
		LoginLimitWindow: time.Minute,
	}
}

// Load parses args (without the program name) against the environment exposed
// by getenv. It returns flag.ErrHelp when the user asked for usage.
func Load(args []string, getenv func(string) string) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := Default()
	fs := flag.NewFlagSet("iptv", flag.ContinueOnError)
	fs.Usage = func() { printUsage(fs.Output(), fs) }

	fs.StringVar(&cfg.Addr, "addr", envString(getenv, "IPTV_ADDR", cfg.Addr), "HTTP listen address")
	fs.DurationVar(&cfg.ReadHeaderTimeout, "read-header-timeout", envDuration(getenv, "IPTV_READ_HEADER_TIMEOUT", cfg.ReadHeaderTimeout), "timeout for reading request headers (slowloris guard)")
	fs.DurationVar(&cfg.ReadTimeout, "read-timeout", envDuration(getenv, "IPTV_READ_TIMEOUT", cfg.ReadTimeout), "timeout for reading a request")
	fs.DurationVar(&cfg.WriteTimeout, "write-timeout", envDuration(getenv, "IPTV_WRITE_TIMEOUT", cfg.WriteTimeout), "timeout for writing a response")
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", envDuration(getenv, "IPTV_IDLE_TIMEOUT", cfg.IdleTimeout), "timeout for idle keep-alive connections")
	fs.DurationVar(&cfg.RequestTimeout, "request-timeout", envDuration(getenv, "IPTV_REQUEST_TIMEOUT", cfg.RequestTimeout), "per-request context budget for handlers")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", envDuration(getenv, "IPTV_SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout), "how long to wait for in-flight requests on SIGTERM")
	fs.Int64Var(&cfg.MaxBodyBytes, "max-body", envInt64(getenv, "IPTV_MAX_BODY_BYTES", cfg.MaxBodyBytes), "maximum request body size in bytes")

	fs.StringVar(&cfg.DatabasePath, "db", envString(getenv, "IPTV_DB", cfg.DatabasePath), "path to the SQLite database file (a directory is created if missing)")
	fs.IntVar(&cfg.DatabaseMaxConns, "db-max-conns", envInt(getenv, "IPTV_DB_MAX_CONNS", cfg.DatabaseMaxConns), "maximum open database connections")
	fs.BoolVar(&cfg.Seed, "seed", envBool(getenv, "IPTV_SEED", cfg.Seed), "insert demo users and channels into an empty database")

	fs.Var(&logLevelFlag{level: &cfg.LogLevel}, "log-level", envString(getenv, "IPTV_LOG_LEVEL", "debug|info|warn|error (default info)"))
	fs.StringVar(&cfg.LogFormat, "log-format", envString(getenv, "IPTV_LOG_FORMAT", cfg.LogFormat), "log format: json or text")

	fs.StringVar(&cfg.StreamBaseURL, "stream-base-url", envString(getenv, "IPTV_STREAM_BASE_URL", cfg.StreamBaseURL), "origin of the streaming server used to build playback URLs")
	fs.BoolVar(&cfg.StreamSigning, "stream-signing", envBool(getenv, "IPTV_STREAM_SIGNING", cfg.StreamSigning), "sign playback URLs with an HMAC so the edge can reject stolen links")
	fs.StringVar(&cfg.StreamKey, "stream-key", envString(getenv, "IPTV_STREAM_KEY", cfg.StreamKey), "HMAC key for playback URLs; generated at startup when empty")
	fs.DurationVar(&cfg.StreamTTL, "stream-ttl", envDuration(getenv, "IPTV_STREAM_TTL", cfg.StreamTTL), "lifetime of a playback URL")

	fs.DurationVar(&cfg.CacheTTL, "cache-ttl", envDuration(getenv, "IPTV_CACHE_TTL", cfg.CacheTTL), "how long a cached response may be served before it is rebuilt")
	fs.DurationVar(&cfg.CacheVersionTTL, "cache-version-ttl", envDuration(getenv, "IPTV_CACHE_VERSION_TTL", cfg.CacheVersionTTL), "how often the data version is re-read from the database; bounds cache invalidation across instances")
	fs.DurationVar(&cfg.CacheStaleTTL, "cache-stale-ttl", envDuration(getenv, "IPTV_CACHE_STALE_TTL", cfg.CacheStaleTTL), "how long a stale entry may still be served when the database fails")
	fs.IntVar(&cfg.CacheMaxEntries, "cache-max-entries", envInt(getenv, "IPTV_CACHE_MAX_ENTRIES", cfg.CacheMaxEntries), "maximum number of cached entries per cache")
	fs.DurationVar(&cfg.AuthCacheTTL, "auth-cache-ttl", envDuration(getenv, "IPTV_AUTH_CACHE_TTL", cfg.AuthCacheTTL), "how long a verified set of credentials is remembered (bounds deactivation latency and password hashing cost)")

	fs.IntVar(&cfg.LoginLimitPerIP, "login-limit", envInt(getenv, "IPTV_LOGIN_LIMIT", cfg.LoginLimitPerIP), "failed logins allowed per client per window, 0 disables the limit")
	fs.DurationVar(&cfg.LoginLimitWindow, "login-window", envDuration(getenv, "IPTV_LOGIN_WINDOW", cfg.LoginLimitWindow), "window for the failed login limit")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate reports configuration that would fail at runtime, so the service
// refuses to start instead of failing on the first request.
func (c *Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("addr must not be empty")
	}
	if c.DatabasePath == "" {
		return fmt.Errorf("db must not be empty")
	}
	if c.DatabaseMaxConns < 1 {
		return fmt.Errorf("db-max-conns must be >= 1, got %d", c.DatabaseMaxConns)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("log-format must be json or text, got %q", c.LogFormat)
	}
	u, err := url.Parse(c.StreamBaseURL)
	if err != nil {
		return fmt.Errorf("stream-base-url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("stream-base-url must be an http(s) origin, got %q", c.StreamBaseURL)
	}
	if u.Host == "" {
		return fmt.Errorf("stream-base-url must include a host, got %q", c.StreamBaseURL)
	}
	if u.Query().Encode() != "" {
		return fmt.Errorf("stream-base-url must not carry a query string: %q", c.StreamBaseURL)
	}
	if c.StreamTTL <= 0 {
		return fmt.Errorf("stream-ttl must be > 0")
	}
	if c.StreamSigning && len(c.StreamKey) < 16 {
		// Not fatal: main() generates an ephemeral key and warns about it.
		c.StreamKey = ""
	}
	if c.CacheTTL <= 0 || c.CacheVersionTTL <= 0 || c.CacheStaleTTL <= 0 {
		return fmt.Errorf("cache ttls must be > 0")
	}
	if c.CacheMaxEntries < 16 {
		return fmt.Errorf("cache-max-entries must be >= 16, got %d", c.CacheMaxEntries)
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request-timeout must be > 0")
	}
	return nil
}

// Logger builds the structured logger. Request credentials never reach it: the
// HTTP layer logs a fixed allow list of fields.
func (c *Config) Logger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.LogLevel}
	if c.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func printUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(w, "Usage: iptv [flags]")
	fmt.Fprintln(w, "\nEvery flag can also be set through the matching IPTV_* environment variable.")
	fmt.Fprintln(w, "\nFlags:")
	fs.PrintDefaults()
}

func envString(getenv func(string) string, name, def string) string {
	if v := getenv(name); v != "" {
		return v
	}
	return def
}

func envBool(getenv func(string) string, name string, def bool) bool {
	if v := getenv(name); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envInt(getenv func(string) string, name string, def int) int {
	if v := getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(getenv func(string) string, name string, def int64) int64 {
	if v := getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envDuration(getenv func(string) string, name string, def time.Duration) time.Duration {
	if v := getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// logLevelFlag adapts slog.Level to flag.Value so that -log-level can report a
// useful message for a bad value.
type logLevelFlag struct{ level *slog.Level }

func (f *logLevelFlag) String() string {
	if f == nil || f.level == nil {
		return "info"
	}
	return f.level.String()
}

func (f *logLevelFlag) Set(s string) error {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToLower(s))); err != nil {
		return fmt.Errorf("log level must be debug, info, warn or error")
	}
	*f.level = lvl
	return nil
}
