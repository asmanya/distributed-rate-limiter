package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// policyFile gives Load a file that exists; the real default does not yet.
func policyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policies.yaml")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"POLICY_FILE": policyFile(t)}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8080 || cfg.MetricsPort != 9090 ||
		cfg.RedisURL != "redis://localhost:6379/0" ||
		cfg.RedisTimeout != 50*time.Millisecond ||
		cfg.AdminEnabled ||
		cfg.LogLevel != slog.LevelInfo ||
		cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("unexpected defaults: %v", cfg)
	}
}

func TestLoadCustom(t *testing.T) {
	file := policyFile(t)
	cfg, err := Load(env(map[string]string{
		"PORT":             "9000",
		"METRICS_PORT":     "9100",
		"REDIS_URL":        "rediss://redis.example.com:6380/1",
		"REDIS_TIMEOUT":    "200ms",
		"POLICY_FILE":      file,
		"ADMIN_ENABLED":    "true",
		"LOG_LEVEL":        "WARN",
		"SHUTDOWN_TIMEOUT": "5s",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9000 || cfg.MetricsPort != 9100 || cfg.RedisTimeout != 200*time.Millisecond ||
		cfg.PolicyFile != file || !cfg.AdminEnabled || cfg.LogLevel != slog.LevelWarn ||
		cfg.ShutdownTimeout != 5*time.Second || cfg.RedisHost() != "redis.example.com:6380" {
		t.Fatalf("unexpected config: %v", cfg)
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantVar string
	}{
		{"port not a number", map[string]string{"PORT": "abc"}, "PORT"},
		{"port zero", map[string]string{"PORT": "0"}, "PORT"},
		{"port too large", map[string]string{"PORT": "70000"}, "PORT"},
		{"metrics port equals port", map[string]string{"PORT": "8080", "METRICS_PORT": "8080"}, "METRICS_PORT"},
		{"redis url wrong scheme", map[string]string{"REDIS_URL": "http://localhost:6379"}, "REDIS_URL"},
		{"redis url garbage", map[string]string{"REDIS_URL": "not a url"}, "REDIS_URL"},
		{"timeout not a duration", map[string]string{"REDIS_TIMEOUT": "fast"}, "REDIS_TIMEOUT"},
		{"timeout below 1ms", map[string]string{"REDIS_TIMEOUT": "100us"}, "REDIS_TIMEOUT"},
		{"timeout above 1s", map[string]string{"REDIS_TIMEOUT": "2s"}, "REDIS_TIMEOUT"},
		{"policy file missing", map[string]string{"POLICY_FILE": "/no/such/file.yaml"}, "POLICY_FILE"},
		{"admin not a bool", map[string]string{"ADMIN_ENABLED": "maybe"}, "ADMIN_ENABLED"},
		{"unknown log level", map[string]string{"LOG_LEVEL": "loud"}, "LOG_LEVEL"},
		{"shutdown timeout zero", map[string]string{"SHUTDOWN_TIMEOUT": "0s"}, "SHUTDOWN_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := map[string]string{"POLICY_FILE": policyFile(t)}
			for k, v := range tt.env {
				e[k] = v
			}
			_, err := Load(env(e))
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("err = %v, want ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), tt.wantVar) {
				t.Fatalf("err = %q, want it to name %s", err, tt.wantVar)
			}
		})
	}
}

func TestLoadReportsAllErrorsAtOnce(t *testing.T) {
	_, err := Load(env(map[string]string{
		"POLICY_FILE":   policyFile(t),
		"PORT":          "abc",
		"REDIS_TIMEOUT": "fast",
		"LOG_LEVEL":     "loud",
	}))
	if err == nil {
		t.Fatal("want an error")
	}
	for _, name := range []string{"PORT", "REDIS_TIMEOUT", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("err = %q, missing %s", err, name)
		}
	}
}

func TestRedisPasswordNeverLeaks(t *testing.T) {
	const secret = "s3cret"
	file := policyFile(t)

	cfg, err := Load(env(map[string]string{
		"POLICY_FILE": file,
		"REDIS_URL":   "rediss://user:" + secret + "@redis.example.com:6379/0",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("loaded", "config", cfg)
	outputs := map[string]string{
		"slog": buf.String(),
		"%v":   fmt.Sprintf("%v", cfg),
		"%+v":  fmt.Sprintf("%+v", cfg),
	}
	for how, out := range outputs {
		if strings.Contains(out, secret) {
			t.Errorf("%s output leaks the password: %s", how, out)
		}
		if !strings.Contains(out, "redis.example.com:6379") {
			t.Errorf("%s output lost the redis host: %s", how, out)
		}
	}

	// An invalid URL's parse error would quote the URL; ours must not.
	_, err = Load(env(map[string]string{
		"POLICY_FILE": file,
		"REDIS_URL":   "redis://user:" + secret + "@host:notaport/0",
	}))
	if err == nil {
		t.Fatal("want an error for a bad URL")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks the password: %v", err)
	}
}
