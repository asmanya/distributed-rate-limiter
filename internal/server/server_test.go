package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
	"github.com/asmanya/distributed-rate-limiter/internal/limiter"
	"github.com/asmanya/distributed-rate-limiter/internal/metrics"
	"github.com/asmanya/distributed-rate-limiter/internal/policy"
	"github.com/asmanya/distributed-rate-limiter/internal/service"
)

const (
	rateLimitName = "ratelimiter.v1.RateLimitService"
	adminName     = "ratelimiter.v1.AdminService"
	healthName    = "grpc.health.v1.Health"
)

// allowAll is a fake limiter: every request is allowed. The server tests are
// about wiring, not about limiting.
type allowAll struct{}

func (allowAll) Allow(_ context.Context, r limiter.Request) (limiter.Decision, error) {
	return limiter.Decision{Allowed: true, Limit: r.Limit, Remaining: r.Limit - 1}, nil
}

func (allowAll) Peek(ctx context.Context, r limiter.Request) (limiter.Decision, error) {
	return allowAll{}.Allow(ctx, r)
}

func (allowAll) Reset(context.Context, limiter.Request) error { return nil }

type onePolicy struct{}

func (onePolicy) Get(name string) (policy.Policy, bool) {
	if name != "search" {
		return policy.Policy{}, false
	}
	return policy.Policy{Name: "search", Algorithm: policy.TokenBucket, Limit: 10, Period: time.Minute}, true
}

// blockLimiter holds every Allow until release is closed, so a test can keep a
// request in flight.
type blockLimiter struct {
	allowAll
	started chan struct{}
	release chan struct{}
}

func (b blockLimiter) Allow(ctx context.Context, r limiter.Request) (limiter.Decision, error) {
	b.started <- struct{}{}
	<-b.release
	return b.allowAll.Allow(ctx, r)
}

// panicLimiter panics on Allow, to prove the interceptor chain turns it into
// Internal and still records it.
type panicLimiter struct{ allowAll }

func (panicLimiter) Allow(context.Context, limiter.Request) (limiter.Decision, error) {
	panic("boom")
}

// syncBuffer is a log sink the server goroutines write and the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newMetrics(t *testing.T) *metrics.Metrics {
	t.Helper()
	m, err := metrics.New()
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	return m
}

func services(adminEnabled bool, l limiter.Limiter, m *metrics.Metrics, log *slog.Logger) Services {
	limiters := map[policy.Algorithm]limiter.Limiter{policy.TokenBucket: l}
	svc := Services{RateLimit: service.NewRateLimit(onePolicy{}, limiters, time.Now, log, m)}
	if adminEnabled {
		svc.Admin = service.NewAdmin(onePolicy{}, limiters, time.Now, log)
	}
	return svc
}

// env is a running server with the pieces a test wants to inspect.
type env struct {
	srv     *Server
	conn    *grpc.ClientConn
	metrics *metrics.Metrics
	logs    *syncBuffer
}

// startEnv runs a server on an in-memory listener and returns a client connected
// to it. No port is opened.
func startEnv(t *testing.T, adminEnabled bool, l limiter.Limiter) env {
	t.Helper()
	m := newMetrics(t)
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	srv := New(services(adminEnabled, l, m, log), m, log)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.ServeGRPC(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return env{srv: srv, conn: conn, metrics: m, logs: logs}
}

func start(t *testing.T, adminEnabled bool) *grpc.ClientConn {
	t.Helper()
	return startEnv(t, adminEnabled, allowAll{}).conn
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestAllowThroughTheServer(t *testing.T) {
	client := ratelimiterv1.NewRateLimitServiceClient(start(t, false))

	resp, err := client.Allow(ctx(t), &ratelimiterv1.AllowRequest{
		Namespace: "orders-api", Policy: "search", Key: "user-1", Cost: 1,
	})
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !resp.GetAllowed() || resp.GetLimit() != 10 || resp.GetRemaining() != 9 {
		t.Fatalf("got %v, want allowed, limit 10, remaining 9", resp)
	}

	_, err = client.Allow(ctx(t), &ratelimiterv1.AllowRequest{Namespace: "orders-api", Policy: "nope", Key: "u"})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("unknown policy: code = %v, want NotFound", got)
	}
}

func TestHealth(t *testing.T) {
	for _, adminEnabled := range []bool{false, true} {
		conn := start(t, adminEnabled)
		health := healthpb.NewHealthClient(conn)

		for _, name := range []string{"", rateLimitName} { // "" is the whole server
			resp, err := health.Check(ctx(t), &healthpb.HealthCheckRequest{Service: name})
			if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
				t.Errorf("admin=%v health(%q) = %v, %v; want SERVING", adminEnabled, name, resp.GetStatus(), err)
			}
		}

		_, err := health.Check(ctx(t), &healthpb.HealthCheckRequest{Service: adminName})
		if adminEnabled && err != nil {
			t.Errorf("admin enabled: health(admin) = %v, want SERVING", err)
		}
		if !adminEnabled && status.Code(err) != codes.NotFound {
			t.Errorf("admin disabled: health(admin) code = %v, want NotFound", status.Code(err))
		}
	}
}

// What reflection lists is what grpcurl shows, so this is the "admin is not
// even there" check for a public deploy.
func TestReflectionListsServices(t *testing.T) {
	for _, adminEnabled := range []bool{false, true} {
		names := listServices(t, start(t, adminEnabled))

		for _, want := range []string{rateLimitName, healthName} {
			if !slices.Contains(names, want) {
				t.Errorf("admin=%v: %q missing from %v", adminEnabled, want, names)
			}
		}
		if got := slices.Contains(names, adminName); got != adminEnabled {
			t.Errorf("admin=%v: admin listed = %v (%v)", adminEnabled, got, names)
		}
	}
}

func listServices(t *testing.T, conn *grpc.ClientConn) []string {
	t.Helper()
	stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx(t))
	if err != nil {
		t.Fatalf("ServerReflectionInfo: %v", err)
	}
	err = stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	_ = stream.CloseSend()

	var names []string
	for _, s := range resp.GetListServicesResponse().GetService() {
		names = append(names, s.GetName())
	}
	return names
}

func TestMetricsEndpoint(t *testing.T) {
	m := newMetrics(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(services(false, allowAll{}, m, log), m, log)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeMetrics(lis) }()
	t.Cleanup(srv.Stop)

	// A Vec series only shows once it has been touched.
	m.Decisions.WithLabelValues("search", "allowed").Inc()

	req, err := http.NewRequestWithContext(ctx(t), http.MethodGet, "http://"+lis.Addr().String()+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"go_goroutines", "ratelimiter_decisions_total"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

func TestPprofOnlyWhenEnabled(t *testing.T) {
	tests := []struct {
		name   string
		pprof  bool
		status int
	}{
		{"off", false, http.StatusNotFound},
		{"on", true, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMetrics(t)
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			svc := services(false, allowAll{}, m, log)
			svc.Pprof = tt.pprof
			srv := New(svc, m, log)
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go func() { _ = srv.ServeMetrics(lis) }()
			t.Cleanup(srv.Stop)

			req, err := http.NewRequestWithContext(ctx(t), http.MethodGet, "http://"+lis.Addr().String()+"/debug/pprof/cmdline", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET /debug/pprof/cmdline: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
		})
	}
}

// The plan's TRAP test: with recovery innermost, a panic still reaches the
// logging and metrics interceptors as Internal.
func TestPanicIsRecoveredLoggedAndCounted(t *testing.T) {
	e := startEnv(t, false, panicLimiter{})
	client := ratelimiterv1.NewRateLimitServiceClient(e.conn)

	_, err := client.Allow(ctx(t), &ratelimiterv1.AllowRequest{Namespace: "orders-api", Policy: "search", Key: "user-1"})
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("code = %v (%v), want Internal", got, err)
	}

	method := "/ratelimiter.v1.RateLimitService/Allow"
	if got := testutil.ToFloat64(e.metrics.RPCHandled.WithLabelValues(method, "Internal")); got != 1 {
		t.Errorf("handled{Internal} = %v, want 1: a panic must be visible in metrics", got)
	}
	out := e.logs.String()
	for _, want := range []string{"panic recovered", "code=Internal", "level=ERROR"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q: %s", want, out)
		}
	}

	// The process is still serving.
	if _, err := ratelimiterv1.NewRateLimitServiceClient(e.conn).Peek(ctx(t),
		&ratelimiterv1.PeekRequest{Namespace: "orders-api", Policy: "search", Key: "user-1"}); err != nil {
		t.Errorf("Peek after panic: %v", err)
	}
}

func shutdownAsync(srv *Server, timeout time.Duration) <-chan error {
	out := make(chan error, 1)
	go func() {
		c, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out <- srv.Shutdown(c)
	}()
	return out
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second): // a failure guard, not a wait the test relies on
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func TestShutdownIdle(t *testing.T) {
	e := startEnv(t, false, allowAll{})
	if err := waitFor(t, shutdownAsync(e.srv, 5*time.Second), "Shutdown"); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// Zero dropped in-flight requests: Shutdown waits for the running RPC and the
// client gets its answer.
func TestShutdownLetsInFlightFinish(t *testing.T) {
	b := blockLimiter{allowAll: allowAll{}, started: make(chan struct{}), release: make(chan struct{})}
	e := startEnv(t, false, b)
	client := ratelimiterv1.NewRateLimitServiceClient(e.conn)

	type result struct {
		resp *ratelimiterv1.AllowResponse
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := client.Allow(ctx(t), &ratelimiterv1.AllowRequest{Namespace: "orders-api", Policy: "search", Key: "user-1"})
		got <- result{resp, err}
	}()
	waitFor(t, b.started, "the request to start")

	sd := shutdownAsync(e.srv, 5*time.Second)
	// An absence check: Shutdown must not return while a request is in flight.
	// It can only miss a bug (pass by luck), never fail a correct server.
	select {
	case err := <-sd:
		t.Fatalf("Shutdown returned with a request in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(b.release)

	r := waitFor(t, got, "the in-flight response")
	if r.err != nil || !r.resp.GetAllowed() {
		t.Fatalf("in-flight request = %v, %v; want allowed, nil", r.resp, r.err)
	}
	if err := waitFor(t, sd, "Shutdown"); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// A Watch stream never ends by itself, so graceful stop cannot finish: the
// timeout must force it. It also shows health flipping to NOT_SERVING.
func TestShutdownFlipsHealthAndForcesOnTimeout(t *testing.T) {
	e := startEnv(t, false, allowAll{})
	wctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := healthpb.NewHealthClient(e.conn).Watch(wctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("first = %v, %v; want SERVING", first.GetStatus(), err)
	}

	sd := shutdownAsync(e.srv, 100*time.Millisecond)

	next, err := stream.Recv()
	if err != nil || next.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("after shutdown = %v, %v; want NOT_SERVING", next.GetStatus(), err)
	}
	if err := waitFor(t, sd, "Shutdown"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want a forced stop wrapping DeadlineExceeded", err)
	}
}

func TestShutdownStopsMetricsServer(t *testing.T) {
	m := newMetrics(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(services(false, allowAll{}, m, log), m, log)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.ServeMetrics(lis) }()
	url := "http://" + lis.Addr().String() + "/metrics"

	get := func() error {
		req, err := http.NewRequestWithContext(ctx(t), http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
	if err := get(); err != nil {
		t.Fatalf("before Shutdown: %v", err)
	}

	if err := waitFor(t, shutdownAsync(srv, 5*time.Second), "Shutdown"); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := waitFor(t, served, "ServeMetrics to return"); err != nil {
		t.Errorf("ServeMetrics = %v, want nil after Shutdown", err)
	}
	if err := get(); err == nil {
		t.Error("metrics server still answers after Shutdown")
	}
}
