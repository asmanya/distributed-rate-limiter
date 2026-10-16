// Package metrics defines every Prometheus collector the service records to, on
// a registry of its own.
//
// Label values are always bounded sets (method, gRPC code, policy name, result,
// script). The request key, user id or IP are never labels: each distinct value
// would become a new time series.
package metrics

import (
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the registry and the collectors that code records to.
type Metrics struct {
	Registry *prometheus.Registry

	// RPCHandled counts finished RPCs by method and gRPC code.
	RPCHandled *prometheus.CounterVec
	// RPCDuration is end-to-end RPC latency by method.
	RPCDuration *prometheus.HistogramVec
	// Decisions counts rate limit decisions by policy and result (allowed, denied).
	Decisions *prometheus.CounterVec
	// RedisDuration is the Redis round trip by script (tb_allow, tb_peek, ...).
	RedisDuration *prometheus.HistogramVec
	// RedisErrors counts store failures by script.
	RedisErrors *prometheus.CounterVec
	// PolicyReloads counts policy reloads by result (success, failure).
	PolicyReloads *prometheus.CounterVec
}

// New builds a fresh registry with the Go and process collectors and every
// service collector registered. Each call has its own registry, so tests can
// call it freely.
func New() (*Metrics, error) {
	// Seconds, 0.5ms to 250ms. The default buckets start at 5ms, which would put
	// every call to a local Redis into the first bucket.
	redisBuckets := []float64{.0005, .001, .002, .005, .01, .025, .05, .1, .25}

	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		RPCHandled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "grpc_server_handled_total",
			Help: "RPCs completed, by method and gRPC code.",
		}, []string{"method", "code"}),
		RPCDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "grpc_server_handling_seconds",
			Help:    "End-to-end RPC latency, by method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method"}),
		Decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ratelimiter_decisions_total",
			Help: "Rate limit decisions, by policy and result.",
		}, []string{"policy", "result"}),
		RedisDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ratelimiter_redis_duration_seconds",
			Help:    "Redis round trip, by script.",
			Buckets: redisBuckets,
		}, []string{"script"}),
		RedisErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ratelimiter_redis_errors_total",
			Help: "Redis failures, by script.",
		}, []string{"script"}),
		PolicyReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ratelimiter_policy_reloads_total",
			Help: "Policy reload attempts, by result.",
		}, []string{"result"}),
	}

	// Register returns an error where MustRegister would panic; library code
	// does not panic.
	for _, c := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.RPCHandled, m.RPCDuration, m.Decisions,
		m.RedisDuration, m.RedisErrors, m.PolicyReloads,
	} {
		if err := m.Registry.Register(c); err != nil {
			return nil, fmt.Errorf("register collector: %w", err)
		}
	}
	return m, nil
}

// Handler serves /metrics from this registry only.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}
