package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewLoggerUsesCloudLoggingFields(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf, slog.LevelInfo).Warn("hello", "policy", "search")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if got["severity"] != "WARN" || got["message"] != "hello" {
		t.Errorf("severity/message = %v/%v, want WARN/hello", got["severity"], got["message"])
	}
	for _, old := range []string{"level", "msg"} {
		if _, ok := got[old]; ok {
			t.Errorf("%q still present: %s", old, buf.String())
		}
	}
	if got["policy"] != "search" {
		t.Errorf("custom attribute lost: %s", buf.String())
	}
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf, slog.LevelWarn).Info("quiet")
	if buf.Len() != 0 {
		t.Errorf("info logged at warn level: %s", buf.String())
	}
}

// go-redis takes its logger as an interface from an internal package we cannot
// import; this keeps the method signature from drifting.
var _ interface {
	Printf(ctx context.Context, format string, v ...any)
} = redisLogger{}

func TestRedisLogger(t *testing.T) {
	var buf bytes.Buffer
	redisLogger{log: newLogger(&buf, slog.LevelInfo)}.Printf(context.Background(),
		"redis: pool %s after %d attempts\n", "failed", 3)

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if got["message"] != "redis: pool failed after 3 attempts" {
		t.Errorf("message = %q, want the formatted text without a trailing newline", got["message"])
	}
	if got["severity"] != "WARN" {
		t.Errorf("severity = %v, want WARN", got["severity"])
	}
	if got["source"] != "go-redis" {
		t.Errorf("source = %v, want go-redis", got["source"])
	}
}

func TestRedisLoggerLevels(t *testing.T) {
	tests := []struct {
		name     string
		level    slog.Level
		wantLine bool
	}{
		{"warn level shows it", slog.LevelWarn, true},
		{"error level hides it", slog.LevelError, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			redisLogger{log: newLogger(&buf, tt.level)}.Printf(context.Background(), "redis: x")
			if got := buf.Len() > 0; got != tt.wantLine {
				t.Errorf("line written = %v, want %v", got, tt.wantLine)
			}
		})
	}
}
