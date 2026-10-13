// Command iptv is a small IPTV/OTT backend: it stores users, channels and
// subscriptions in SQLite and serves them over a REST API protected by HTTP
// Basic Auth.
//
// The binary is self contained: the schema is created on first start, demo data
// is inserted into an empty database, the admin panel is served from the same
// process, and SIGTERM drains in-flight requests before the database handle is
// closed.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"iptv/internal/api"
	"iptv/internal/auth"
	"iptv/internal/cache"
	"iptv/internal/config"
	"iptv/internal/limit"
	"iptv/internal/panel"
	"iptv/internal/seed"
	"iptv/internal/service"
	"iptv/internal/store"
	"iptv/internal/stream"
)

// startupTimeout bounds migrations and seeding so a locked or broken database
// fails the start instead of hanging it.
const startupTimeout = 30 * time.Second

func main() {
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "iptv:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:], os.Getenv)
	if err != nil {
		return err
	}
	if cfg.Healthcheck {
		return healthcheck(cfg)
	}
	log := cfg.Logger()

	if err := prepareDataDir(cfg.DatabasePath); err != nil {
		return err
	}

	// SIGTERM (containers, systemd) and SIGINT (a person at the keyboard) start
	// the same graceful stop.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	if err := ctx.Err(); err != nil {
		return nil // signalled before we even started
	}

	startCtx, cancelStart := context.WithTimeout(ctx, startupTimeout)
	defer cancelStart()

	st, err := store.Open(startCtx, store.DSN(cfg.DatabasePath), log,
		store.WithMaxConns(cfg.DatabaseMaxConns))
	if err != nil {
		return err
	}
	// Closed after the HTTP server has drained, because defer order puts this
	// last: no handler can reach a closed handle.
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("could not close database", "error", err)
		}
	}()

	if cfg.Seed {
		seeded, err := seed.Demo(startCtx, st, log)
		if err != nil {
			return err
		}
		if !seeded {
			log.Debug("seed skipped, database is not empty")
		}
	}

	signer, err := newSigner(cfg, log)
	if err != nil {
		return err
	}

	// One version reader for the whole process: with it a fleet of instances
	// sees an administrator's change within CacheVersionTTL without talking to
	// each other.
	versions := cache.NewVersions(st, cfg.CacheVersionTTL, log)

	limiter := limit.New(cfg.LoginLimitPerIP, cfg.LoginLimitWindow)
	// Credentials are verified with an expensive KDF, so they are cached for a
	// few seconds. The stale window for that cache is kept short on purpose: it
	// is the maximum time a disabled account could still be let in if the
	// database became unreachable at the wrong moment.
	authStale := min(cfg.CacheStaleTTL, 30*time.Second)
	authenticator, err := auth.New(st, versions, cfg.AuthCacheTTL, authStale, cfg.CacheMaxEntries, limiter, log)
	if err != nil {
		return err
	}

	channels := service.NewChannels(st, versions, cfg.CacheTTL, cfg.CacheStaleTTL,
		cfg.CacheMaxEntries, signer, log)
	adminService := service.NewAdmin(st, channels, log)

	panelHandler, err := panel.New()
	if err != nil {
		return err
	}

	router := api.NewRouter(api.HandlerConfig{
		Channels:       channels,
		Admin:          adminService,
		Auth:           auth.NewMiddleware(authenticator, log),
		AuthCache:      authenticator,
		DB:             st,
		Logger:         log,
		RequestTimeout: cfg.RequestTimeout,
		MaxBodyBytes:   cfg.MaxBodyBytes,
		Panel:          panelHandler,
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		// The library's own complaints (malformed requests, TLS handshakes) go
		// through the same structured log, at a level that stays out of the way.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening",
			"addr", cfg.Addr,
			"db", cfg.DatabasePath,
			"log_format", cfg.LogFormat,
			"log_level", cfg.LogLevel.String(),
			"stream_signing", cfg.StreamSigning,
			"cache_ttl", cfg.CacheTTL.String(),
			"cache_version_ttl", cfg.CacheVersionTTL.String(),
			"admin_panel", "/admin/",
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	// Graceful stop: stop accepting new connections, wait for the requests that
	// are already running, then release the database. The context that carries
	// the shutdown signal is already cancelled, so the drain gets its own.
	log.Info("shutting down", "in_flight_budget", cfg.ShutdownTimeout.String())
	srv.SetKeepAlivesEnabled(false)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := srv.Close(); err != nil {
		return fmt.Errorf("close server: %w", err)
	}
	log.Info("stopped")
	return nil
}

// newSigner prepares the playback link signer. Without an explicit key it
// generates one for this process: signing stays on by default, but links from
// an earlier run — or from a second instance — will not verify, which is why the
// fallback is logged loudly.
func newSigner(cfg *config.Config, log *slog.Logger) (*stream.Signer, error) {
	key := []byte(cfg.StreamKey)
	if cfg.StreamSigning && len(key) == 0 {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generate stream key: %w", err)
		}
		key = []byte(hex.EncodeToString(raw))
		log.Warn("no stream key configured, generated an ephemeral one",
			"hint", "set IPTV_STREAM_KEY so links survive a restart and work across instances")
	}
	return stream.New(cfg.StreamBaseURL, key, cfg.StreamTTL, cfg.StreamSigning)
}

// healthcheck lets the container image probe its own process without shipping a
// shell, curl or python in the final layer: the binary is the probe.
func healthcheck(cfg *config.Config) error {
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return fmt.Errorf("healthcheck: cannot derive an address from %q: %w", cfg.Addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+net.JoinHostPort(host, port)+cfg.HealthcheckPath, nil)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s answered %s", cfg.HealthcheckPath, resp.Status)
	}
	return nil
}

// prepareDataDir creates the directory of a file based database. The default
// configuration points at ./data, and failing with "unable to open database
// file" on a fresh clone is a poor first minute.
func prepareDataDir(path string) error {
	if path == ":memory:" {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create database directory %q: %w", dir, err)
	}
	return nil
}
