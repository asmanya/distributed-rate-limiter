package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/asmanya/distributed-rate-limiter/internal/metrics"
	"github.com/asmanya/distributed-rate-limiter/internal/policy"
)

func testMetrics(t *testing.T) *metrics.Metrics {
	t.Helper()
	m, err := metrics.New()
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	return m
}

// signals returns a channel holding n signals and already closed: watchReload
// drains them and then returns, so tests need no sleep and no timing.
func signals(n int) chan os.Signal {
	ch := make(chan os.Signal, n)
	for range n {
		ch <- os.Interrupt
	}
	close(ch)
	return ch
}

func TestWatchReload(t *testing.T) {
	boom := errors.New("bad yaml")

	tests := []struct {
		name        string
		signals     int
		results     []error // what reload returns, call by call
		wantCalls   int
		wantSuccess float64
		wantFailure float64
		wantLog     []string
	}{
		{"no signal", 0, nil, 0, 0, 0, nil},
		{"success", 1, []error{nil}, 1, 1, 0, []string{"policies reloaded"}},
		{"failure", 1, []error{boom}, 1, 0, 1, []string{"level=ERROR", "keeping old policies", "bad yaml"}},
		// A failure must not stop the watcher: the next signal still reloads.
		{"failure then success", 2, []error{boom, nil}, 2, 1, 1, []string{"keeping old policies", "policies reloaded"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testMetrics(t)
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, nil))
			calls := 0
			reload := func() error {
				err := tt.results[calls]
				calls++
				return err
			}

			watchReload(context.Background(), signals(tt.signals), reload, log, m)

			if calls != tt.wantCalls {
				t.Errorf("reload calls = %d, want %d", calls, tt.wantCalls)
			}
			if got := testutil.ToFloat64(m.PolicyReloads.WithLabelValues("success")); got != tt.wantSuccess {
				t.Errorf("reloads{success} = %v, want %v", got, tt.wantSuccess)
			}
			if got := testutil.ToFloat64(m.PolicyReloads.WithLabelValues("failure")); got != tt.wantFailure {
				t.Errorf("reloads{failure} = %v, want %v", got, tt.wantFailure)
			}
			for _, want := range tt.wantLog {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("log missing %q: %s", want, buf.String())
				}
			}
		})
	}
}

func TestWatchReloadStopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sigs := make(chan os.Signal) // never closed, never written: only ctx can end it

	done := make(chan struct{})
	go func() {
		watchReload(ctx, sigs, func() error { return nil }, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), testMetrics(t))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second): // a failure guard, not a wait the test relies on
		t.Fatal("watchReload did not return after the context ended")
	}
}

// The plan's behaviour end to end, with the real store: a signal applies a new
// limit, and a broken file leaves the previous policies serving.
func TestWatchReloadWithRealStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policies.yaml")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policyYAML := func(limit string) string {
		return "policies:\n  - name: search\n    algorithm: token_bucket\n    limit: " + limit + "\n    period: 1m\n"
	}
	limitOf := func(store *policy.Store) int64 {
		t.Helper()
		p, ok := store.Get("search")
		if !ok {
			t.Fatal("policy search missing")
		}
		return p.Limit
	}
	reloadOnce := func(store *policy.Store, m *metrics.Metrics) {
		watchReload(context.Background(), signals(1), func() error { return store.Reload(path) },
			slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), m)
	}

	m := testMetrics(t)
	store := policy.NewStore()
	write(policyYAML("10"))
	if err := store.Reload(path); err != nil {
		t.Fatalf("initial Reload: %v", err)
	}
	if got := limitOf(store); got != 10 {
		t.Fatalf("limit = %d, want 10", got)
	}

	write(policyYAML("20"))
	reloadOnce(store, m)
	if got := limitOf(store); got != 20 {
		t.Errorf("after SIGHUP limit = %d, want 20", got)
	}

	write("policies:\n  - name: search\n    bogus_field: 1\n")
	reloadOnce(store, m)
	if got := limitOf(store); got != 20 {
		t.Errorf("after a broken file limit = %d, want 20 (old policies keep serving)", got)
	}

	if got := testutil.ToFloat64(m.PolicyReloads.WithLabelValues("success")); got != 1 {
		t.Errorf("reloads{success} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.PolicyReloads.WithLabelValues("failure")); got != 1 {
		t.Errorf("reloads{failure} = %v, want 1", got)
	}
}
