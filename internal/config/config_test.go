package config

import (
	"flag"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestDefaultsAreUsableWithoutAnyConfiguration(t *testing.T) {
	cfg, err := Load(nil, env(nil))
	if err != nil {
		t.Fatalf("Load with no arguments and no environment: %v", err)
	}
	if cfg.Addr != ":8080" || cfg.DatabasePath == "" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if cfg.StreamSigning != true {
		t.Error("playback links must be signed unless somebody switches it off deliberately")
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("log level = %v, want info", cfg.LogLevel)
	}
}

func TestEnvironmentOverridesDefaults(t *testing.T) {
	cfg, err := Load(nil, env(map[string]string{
		"IPTV_ADDR":              ":9999",
		"IPTV_DB":                "/tmp/x.db",
		"IPTV_SEED":              "false",
		"IPTV_REQUEST_TIMEOUT":   "2s",
		"IPTV_LOG_LEVEL":         "warn",
		"IPTV_STREAM_TTL":        "1m",
		"IPTV_LOGIN_LIMIT":       "0",
		"IPTV_PBKDF2_ITERATIONS": "5000",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9999" || cfg.DatabasePath != "/tmp/x.db" || cfg.Seed {
		t.Errorf("environment was ignored: %+v", cfg)
	}
	if cfg.RequestTimeout != 2*time.Second {
		t.Errorf("request timeout = %v", cfg.RequestTimeout)
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Errorf("log level = %v", cfg.LogLevel)
	}
	if cfg.LoginLimitPerIP != 0 {
		t.Errorf("login limit = %d, want 0 (disabled)", cfg.LoginLimitPerIP)
	}
}

// A bad value in the environment must be reported, not quietly ignored.
func TestBadEnvironmentValueIsReported(t *testing.T) {
	_, err := Load(nil, env(map[string]string{"IPTV_LOG_LEVEL": "loud"}))
	if err == nil || !strings.Contains(err.Error(), "IPTV_LOG_LEVEL") {
		t.Fatalf("err = %v, want a complaint naming IPTV_LOG_LEVEL", err)
	}
}

func TestFlagsWinOverEnvironment(t *testing.T) {
	cfg, err := Load([]string{"-addr", ":1234", "-log-level", "debug"},
		env(map[string]string{"IPTV_ADDR": ":9999", "IPTV_LOG_LEVEL": "error"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":1234" {
		t.Errorf("addr = %q, want :1234", cfg.Addr)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("log level = %v, want debug", cfg.LogLevel)
	}
}

func TestValidationRejectsWhatWouldFailAtRuntime(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantsIn string
	}{
		{"empty addr", []string{"-addr", ""}, "addr"},
		{"empty db", []string{"-db", ""}, "db"},
		{"bad log format", []string{"-log-format", "yaml"}, "log-format"},
		{"bad log level", []string{"-log-level", "loud"}, "log level"},
		{"stream url without scheme", []string{"-stream-base-url", "stream.example.com"}, "stream-base-url"},
		{"stream url with a query", []string{"-stream-base-url", "http://s.example/?token=1"}, "stream-base-url"},
		{"zero stream ttl", []string{"-stream-ttl", "0s"}, "stream-ttl"},
		{"zero cache ttl", []string{"-cache-ttl", "0s"}, "cache ttls"},
		{"weak kdf", []string{"-pbkdf2-iterations", "10"}, "pbkdf2-iterations"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.args, env(nil))
			if err == nil {
				t.Fatalf("no error for %v", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantsIn) {
				t.Errorf("error %q does not mention %q", err, tc.wantsIn)
			}
		})
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	// Usage goes to stderr while parsing; keep the test output readable.
	devNull, err0 := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err0 != nil {
		t.Fatalf("open devnull: %v", err0)
	}
	defer devNull.Close()
	stderr := os.Stderr
	os.Stderr = devNull
	defer func() { os.Stderr = stderr }()

	if _, err := Load([]string{"-h"}, env(nil)); err != flag.ErrHelp {
		t.Errorf("err = %v, want flag.ErrHelp", err)
	}
}

// The short key is only accepted as an empty value: signing with a key that
// nobody configured must be visible, not silent, and main() is the place that
// decides what to do about it.
func TestShortStreamKeyIsTreatedAsMissing(t *testing.T) {
	cfg, err := Load([]string{"-stream-key", "abc"}, env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StreamKey != "" {
		t.Errorf("a key below the usable length should be reset, got %q", cfg.StreamKey)
	}
}
