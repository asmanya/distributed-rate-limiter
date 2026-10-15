// Package server hosts the gRPC services together with health, reflection and a
// separate HTTP server for metrics. It decides what is exposed, not what the
// services do.
package server

import (
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
)

// Services are the gRPC services to host.
type Services struct {
	RateLimit ratelimiterv1.RateLimitServiceServer
	// Admin is nil unless ADMIN_ENABLED=true. A nil Admin means the service is
	// not registered at all: it does not show up in reflection or health.
	Admin ratelimiterv1.AdminServiceServer
}

// Server is the gRPC server plus the metrics HTTP server. It takes the
// listeners from outside, so tests can use bufconn or a random port.
type Server struct {
	grpc    *grpc.Server
	metrics *http.Server
}

// New registers the services, health and reflection, and routes /metrics to the
// given handler.
func New(svc Services, metrics http.Handler) *Server {
	g := grpc.NewServer()
	h := health.NewServer()

	ratelimiterv1.RegisterRateLimitServiceServer(g, svc.RateLimit)
	h.SetServingStatus(ratelimiterv1.RateLimitService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)

	if svc.Admin != nil {
		ratelimiterv1.RegisterAdminServiceServer(g, svc.Admin)
		h.SetServingStatus(ratelimiterv1.AdminService_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	}

	// Health answers "is this process up", nothing more. It must not depend on
	// Redis: a Redis outage should not make the platform kill and restart us.
	healthpb.RegisterHealthServer(g, h)
	h.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	// Reflection lets grpcurl work without the .proto files.
	reflection.Register(g)

	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics)
	return &Server{
		grpc:    g,
		metrics: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second},
	}
}

// ServeGRPC serves gRPC on l until Stop is called.
func (s *Server) ServeGRPC(l net.Listener) error {
	return s.grpc.Serve(l)
}

// ServeMetrics serves the metrics HTTP endpoint on l until Stop is called.
func (s *Server) ServeMetrics(l net.Listener) error {
	if err := s.metrics.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Stop stops both servers immediately. Graceful shutdown comes in Phase 3.
func (s *Server) Stop() {
	s.grpc.Stop()
	_ = s.metrics.Close()
}

// RuntimeMetrics returns a handler for Go runtime and process metrics, on its
// own registry (not the global default). Domain metrics replace it in Phase 3.
func RuntimeMetrics() (http.Handler, error) {
	reg := prometheus.NewRegistry()
	for _, c := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), nil
}
