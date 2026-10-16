package main

import (
	"bytes"
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
