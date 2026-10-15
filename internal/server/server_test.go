package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
	"github.com/asmanya/distributed-rate-limiter/internal/limiter"
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

func services(adminEnabled bool) Services {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiters := map[policy.Algorithm]limiter.Limiter{policy.TokenBucket: allowAll{}}
	svc := Services{RateLimit: service.NewRateLimit(onePolicy{}, limiters, time.Now, log)}
	if adminEnabled {
		svc.Admin = service.NewAdmin(onePolicy{}, limiters, time.Now, log)
	}
	return svc
}

// start runs a server on an in-memory listener and returns a client connected
// to it. No port is opened.
func start(t *testing.T, adminEnabled bool) *grpc.ClientConn {
	t.Helper()
	metrics, err := RuntimeMetrics()
	if err != nil {
		t.Fatalf("RuntimeMetrics: %v", err)
	}
	srv := New(services(adminEnabled), metrics)
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
	return conn
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
	metrics, err := RuntimeMetrics()
	if err != nil {
		t.Fatalf("RuntimeMetrics: %v", err)
	}
	srv := New(services(false), metrics)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeMetrics(lis) }()
	t.Cleanup(srv.Stop)

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
	if !strings.Contains(string(body), "go_goroutines") {
		t.Fatalf("no Go runtime metrics in the response")
	}
}
