package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape records one sample on every collector and returns the /metrics body.
// Vec series only appear once a label combination has been touched.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	m.RPCHandled.WithLabelValues("/ratelimiter.v1.RateLimitService/Allow", "OK").Inc()
	m.RPCDuration.WithLabelValues("/ratelimiter.v1.RateLimitService/Allow").Observe(0.001)
	m.Decisions.WithLabelValues("search", "allowed").Inc()
	m.RedisDuration.WithLabelValues("tb_allow").Observe(0.0003)
	m.RedisErrors.WithLabelValues("tb_allow").Inc()
	m.PolicyReloads.WithLabelValues("success").Inc()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestNewTwice(t *testing.T) {
	// Each New has its own registry, so a second one must not hit a duplicate
	// registration error.
	for range 2 {
		if _, err := New(); err != nil {
			t.Fatalf("New: %v", err)
		}
	}
}

func TestExposition(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := scrape(t, m)

	tests := []struct {
		name string
		want string
	}{
		{"rpc counter", "grpc_server_handled_total{"},
		{"rpc histogram", "grpc_server_handling_seconds_bucket{"},
		{"decisions", `ratelimiter_decisions_total{policy="search",result="allowed"}`},
		{"redis histogram", "ratelimiter_redis_duration_seconds_bucket{"},
		{"redis errors", `ratelimiter_redis_errors_total{script="tb_allow"}`},
		{"policy reloads", `ratelimiter_policy_reloads_total{result="success"}`},
		{"go runtime", "go_goroutines"},
		{"redis bucket 0.5ms", `ratelimiter_redis_duration_seconds_bucket{script="tb_allow",le="0.0005"}`},
		{"redis bucket 250ms", `ratelimiter_redis_duration_seconds_bucket{script="tb_allow",le="0.25"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(body, tt.want) {
				t.Errorf("/metrics missing %q", tt.want)
			}
		})
	}
}

func TestRegistriesAreIndependent(t *testing.T) {
	a, _ := New()
	b, _ := New()
	a.Decisions.WithLabelValues("search", "denied").Inc()

	if strings.Contains(scrape(t, b), `result="denied"`) {
		t.Error("counter from one registry leaked into another")
	}
}
