package server

import (
	"context"
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
	return env{conn: conn, metrics: m, logs: logs}
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
