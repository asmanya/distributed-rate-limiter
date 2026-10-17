// Package server hosts the gRPC services together with health, reflection and a
// separate HTTP server for metrics. It decides what is exposed, not what the
// services do.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
	"github.com/asmanya/distributed-rate-limiter/internal/metrics"
	"github.com/asmanya/distributed-rate-limiter/internal/server/interceptors"
)

// Services are the gRPC services to host.
type Services struct {
	RateLimit ratelimiterv1.RateLimitServiceServer
	// Admin is nil unless ADMIN_ENABLED=true. A nil Admin means the service is
	// not registered at all: it does not show up in reflection or health.
	Admin ratelimiterv1.AdminServiceServer
	// Pprof serves /debug/pprof/ on the metrics port. It exposes internals and
	// a profile request costs CPU, so it is off unless PPROF_ENABLED=true.
	Pprof bool
}

// Server is the gRPC server plus the metrics HTTP server. It takes the
// listeners from outside, so tests can use bufconn or a random port.
type Server struct {
	grpc    *grpc.Server
	health  *health.Server
	metrics *http.Server
}

// New registers the services, health and reflection, and serves /metrics from m.
func New(svc Services, m *metrics.Metrics, log *slog.Logger) *Server {
	// The first interceptor is the outermost. Logging and metrics sit outside
	// recovery on purpose: recovery turns a panic into an Internal status, so they
	// see the final code and the full duration, and a panic is logged and counted
	// as Internal. With recovery outermost they would be skipped by the panic and
	// panics would be invisible in the metrics.
	g := grpc.NewServer(grpc.ChainUnaryInterceptor(
		interceptors.Logging(log),
		interceptors.Metrics(m),
		interceptors.Recovery(log),
	))
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
	mux.Handle("/metrics", m.Handler())
	if svc.Pprof {
		// Our own mux, so the net/http/pprof init() on DefaultServeMux does
		// not apply: the handlers are registered by hand. No WriteTimeout on
		// the server: a 30 s CPU profile would be cut off.
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return &Server{
		grpc:    g,
		health:  h,
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

// Shutdown drains the server: health goes NOT_SERVING so load balancers stop
// sending traffic, in-flight RPCs finish, then the metrics server stops. If ctx
// ends first, whatever is still running is stopped by force and the error says so.
//
// gRPC drains before the metrics server so metrics can still be scraped while
// the last requests finish.
func (s *Server) Shutdown(ctx context.Context) error {
	// Only for shutdown: a Redis outage never flips health (locked rule 7).
	s.health.Shutdown()

	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()

	var forced error
	select {
	case <-done:
	case <-ctx.Done():
		// GracefulStop waits for every RPC, and a stream (even a health Watch)
		// may never end. Stop makes it return.
		s.grpc.Stop()
		<-done
		forced = fmt.Errorf("graceful stop timed out: %w", ctx.Err())
	}

	if err := s.metrics.Shutdown(ctx); err != nil {
		_ = s.metrics.Close()
	}
	return forced
}

// Stop stops both servers immediately. It is for tests and for a server that
// failed on its own; a normal exit goes through Shutdown.
func (s *Server) Stop() {
	s.grpc.Stop()
	_ = s.metrics.Close()
}
