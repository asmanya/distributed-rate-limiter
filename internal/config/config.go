// Package config reads the service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Config is the validated service configuration.
type Config struct {
	Port        int
	MetricsPort int
	// RedisURL may contain a password. Never log it: log the Config (LogValue
	// redacts it) or use RedisHost.
	RedisURL        string
	RedisTimeout    time.Duration
	PolicyFile      string
	AdminEnabled    bool
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
}

// Load reads and validates the configuration. getenv is os.Getenv in main and a
// map lookup in tests, so tests need no real environment.
//
// It reports every problem at once instead of stopping at the first: with three
// bad variables a deploy would otherwise fail three times, minutes apart.
func Load(getenv func(string) string) (Config, error) {
	var (
		cfg  Config
		errs []error
	)
	fail := func(name, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: %s: %s", ErrInvalidConfig, name, fmt.Sprintf(format, args...)))
	}
	get := func(name, def string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}

	portOK, metricsOK := true, true
	var err error
	if cfg.Port, err = parsePort(get("PORT", "8080")); err != nil {
		fail("PORT", "%v", err)
		portOK = false
	}
	if cfg.MetricsPort, err = parsePort(get("METRICS_PORT", "9090")); err != nil {
		fail("METRICS_PORT", "%v", err)
		metricsOK = false
	}
	if portOK && metricsOK && cfg.Port == cfg.MetricsPort {
		fail("METRICS_PORT", "must differ from PORT (both %d)", cfg.Port)
	}

	cfg.RedisURL = get("REDIS_URL", "redis://localhost:6379/0")
	if _, err := redis.ParseURL(cfg.RedisURL); err != nil {
		// The cause is deliberately dropped: a parse error quotes the URL, and
		// the URL holds the password.
		fail("REDIS_URL", "not a valid redis:// or rediss:// URL")
	}

	cfg.RedisTimeout, err = time.ParseDuration(get("REDIS_TIMEOUT", "50ms"))
	if err != nil || cfg.RedisTimeout < time.Millisecond || cfg.RedisTimeout > time.Second {
		fail("REDIS_TIMEOUT", "must be a duration from 1ms to 1s, got %q", get("REDIS_TIMEOUT", "50ms"))
	}

	cfg.PolicyFile = get("POLICY_FILE", "deploy/policies.yaml")
	if info, err := os.Stat(cfg.PolicyFile); err != nil {
		fail("POLICY_FILE", "cannot read %q", cfg.PolicyFile)
	} else if info.IsDir() {
		fail("POLICY_FILE", "%q is a directory", cfg.PolicyFile)
	}

	if cfg.AdminEnabled, err = strconv.ParseBool(get("ADMIN_ENABLED", "false")); err != nil {
		fail("ADMIN_ENABLED", "must be true or false, got %q", get("ADMIN_ENABLED", "false"))
	}

	level, ok := parseLevel(get("LOG_LEVEL", "info"))
	if !ok {
		fail("LOG_LEVEL", "must be debug, info, warn or error, got %q", get("LOG_LEVEL", "info"))
	}
	cfg.LogLevel = level

	cfg.ShutdownTimeout, err = time.ParseDuration(get("SHUTDOWN_TIMEOUT", "10s"))
	if err != nil || cfg.ShutdownTimeout <= 0 {
		fail("SHUTDOWN_TIMEOUT", "must be a positive duration, got %q", get("SHUTDOWN_TIMEOUT", "10s"))
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

func parsePort(raw string) (int, error) {
	p, err := strconv.Atoi(raw)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("must be an integer from 1 to 65535, got %q", raw)
	}
	return p, nil
}

func parseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// RedisHost is the host:port of REDIS_URL, without credentials. It is the only
// part of the URL that is safe to log.
func (c Config) RedisHost() string {
	u, err := url.Parse(c.RedisURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// LogValue makes slog print the Config without secrets (it is called
// automatically when a Config is logged).
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("port", c.Port),
		slog.Int("metrics_port", c.MetricsPort),
		slog.String("redis_host", c.RedisHost()),
		slog.Duration("redis_timeout", c.RedisTimeout),
		slog.String("policy_file", c.PolicyFile),
		slog.Bool("admin_enabled", c.AdminEnabled),
		slog.String("log_level", c.LogLevel.String()),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
	)
}

// String redacts too, so fmt.Printf("%v", cfg) cannot leak the password either.
func (c Config) String() string {
	return c.LogValue().String()
}
