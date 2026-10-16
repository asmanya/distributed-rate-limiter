package client

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
)

// fakeServer answers Allow with whatever the test says.
type fakeServer struct {
	ratelimiterv1.UnimplementedRateLimitServiceServer
	allow func(context.Context, *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error)
}

func (f fakeServer) Allow(ctx context.Context, r *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error) {
	return f.allow(ctx, r)
}

// validOpts are the required options with test values.
func validOpts(extra ...Option) []Option {
	return append([]Option{
		WithNamespace("orders-api"),
		WithFailMode(FailOpen),
		WithTransportCredentials(insecure.NewCredentials()),
	}, extra...)
}

// startFake runs fake on an in-memory listener and returns a Client connected to
// it. No port is opened.
func startFake(t *testing.T, allow func(context.Context, *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error), extra ...Option) *Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	ratelimiterv1.RegisterRateLimitServiceServer(srv, fakeServer{allow: allow})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cfg, err := newConfig(validOpts(extra...))
	if err != nil {
		t.Fatalf("newConfig: %v", err)
	}
	c := newClient(conn, cfg)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestNewRequiredOptions(t *testing.T) {
	tests := []struct {
		name    string
		opts    []Option
		wantErr []string // every one of these must be in the error
	}{
		{"all set", validOpts(), nil},
		{"no namespace", []Option{WithFailMode(FailOpen), WithTransportCredentials(insecure.NewCredentials())}, []string{"namespace"}},
		{"no credentials", []Option{WithNamespace("a"), WithFailMode(FailOpen)}, []string{"credentials"}},
		{"no fail mode", []Option{WithNamespace("a"), WithTransportCredentials(insecure.NewCredentials())}, []string{"fail mode"}},
		{"nothing set", nil, []string{"namespace", "credentials", "fail mode"}},
		{"unknown fail mode", validOpts(WithFailMode(FailMode(99))), []string{"fail mode"}},
		{"zero timeout", validOpts(WithTimeout(0)), []string{"timeout"}},
		{"negative timeout", validOpts(WithTimeout(-time.Second)), []string{"timeout"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New("localhost:8080", tt.opts...)
			if c != nil {
				t.Cleanup(func() { _ = c.Close() })
			}
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("New returned no error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestAllowReturnsDecision(t *testing.T) {
	tests := []struct {
		name string
		resp *ratelimiterv1.AllowResponse
		want Decision
	}{
		{
			"allowed",
			&ratelimiterv1.AllowResponse{Allowed: true, Limit: 5, Remaining: 4, RetryAfter: durationpb.New(0)},
			Decision{Allowed: true, Remaining: 4},
		},
		{
			// A denial is a normal answer, not an error.
			"denied",
			&ratelimiterv1.AllowResponse{Allowed: false, Limit: 5, Remaining: 0, RetryAfter: durationpb.New(3 * time.Second)},
			Decision{Allowed: false, RetryAfter: 3 * time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := startFake(t, func(context.Context, *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error) {
				return tt.resp, nil
			})

			got, err := c.Allow(context.Background(), "search", "user-1")

			if err != nil {
				t.Fatalf("Allow: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAllowSendsNamespacePolicyKey(t *testing.T) {
	seen := make(chan *ratelimiterv1.AllowRequest, 1)
	c := startFake(t, func(_ context.Context, r *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error) {
		seen <- r
		return &ratelimiterv1.AllowResponse{Allowed: true}, nil
	})

	if _, err := c.Allow(context.Background(), "search", "user-1"); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	r := <-seen
	if r.GetNamespace() != "orders-api" || r.GetPolicy() != "search" || r.GetKey() != "user-1" {
		t.Errorf("server saw %v", r)
	}
	// Cost is left out: the service reads 0 as 1.
	if r.GetCost() != 0 {
		t.Errorf("cost = %d, want 0", r.GetCost())
	}
}

func TestAllowPassesGRPCErrorThrough(t *testing.T) {
	c := startFake(t, func(context.Context, *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error) {
		return nil, status.Error(codes.Unavailable, "rate limit store unavailable")
	})

	_, err := c.Allow(context.Background(), "search", "user-1")

	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("code = %v (%v), want Unavailable", got, err)
	}
}

func TestAllowTimeout(t *testing.T) {
	deadline := make(chan bool, 1)
	c := startFake(t, func(ctx context.Context, _ *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error) {
		_, ok := ctx.Deadline()
		deadline <- ok
		<-ctx.Done() // never answers
		return nil, ctx.Err()
	}, WithTimeout(20*time.Millisecond))

	start := time.Now()
	_, err := c.Allow(context.Background(), "search", "user-1")

	if got := status.Code(err); got != codes.DeadlineExceeded {
		t.Fatalf("code = %v (%v), want DeadlineExceeded", got, err)
	}
	if took := time.Since(start); took > 5*time.Second { // a guard against a hang, not a measurement
		t.Errorf("Allow took %v, the timeout did not apply", took)
	}
	if !<-deadline {
		t.Error("the server saw no deadline: the timeout was not sent")
	}
}

// New does no I/O. A Client for an address nobody listens on is created fine,
// and the failure only shows on the first call, so the fail mode has to be ready
// for it.
func TestNewIsLazy(t *testing.T) {
	c, err := New("127.0.0.1:1", validOpts(WithTimeout(time.Second))...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	_, err = c.Allow(context.Background(), "search", "user-1")

	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("code = %v (%v), want Unavailable", got, err)
	}
}

func TestClose(t *testing.T) {
	c := startFake(t, func(context.Context, *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error) {
		return &ratelimiterv1.AllowResponse{Allowed: true}, nil
	})

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Allow(context.Background(), "search", "user-1"); err == nil {
		t.Error("Allow after Close returned no error")
	}
}
