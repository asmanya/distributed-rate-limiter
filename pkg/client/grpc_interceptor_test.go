package client

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
)

const testMethod = "/orders.v1.Orders/Create"

type allowFunc = func(context.Context, *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error)

// answer is a fake limiter that always gives the same answer, and reports every
// request it receives on seen.
func answer(resp *ratelimiterv1.AllowResponse, err error, seen chan<- *ratelimiterv1.AllowRequest) allowFunc {
	return func(_ context.Context, r *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error) {
		if seen != nil {
			seen <- r
		}
		return resp, err
	}
}

func allowed() *ratelimiterv1.AllowResponse { return &ratelimiterv1.AllowResponse{Allowed: true} }

// spy is the protected handler: it records whether it ran.
type spy struct{ called bool }

func (s *spy) handle(context.Context, any) (any, error) {
	s.called = true
	return "ok", nil
}

// captureLogs makes slog's default logger write to the returned buffer for the
// rest of the test. The default logger is process-wide, so these tests do not run
// in parallel.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func intercept(ctx context.Context, c *Client, key KeyFunc, h *spy) (any, error) {
	policies := map[string]string{testMethod: "create-order"}
	return c.UnaryServerInterceptor(policies, key)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: testMethod}, h.handle)
}

func TestInterceptorMethodNotInMap(t *testing.T) {
	seen := make(chan *ratelimiterv1.AllowRequest, 1)
	c := startFake(t, answer(allowed(), nil, seen))
	h := &spy{}

	policies := map[string]string{testMethod: "create-order"}
	_, err := c.UnaryServerInterceptor(policies, nil)(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}, h.handle)

	if err != nil || !h.called {
		t.Fatalf("handler called = %v, err = %v; want it to run", h.called, err)
	}
	select {
	case r := <-seen:
		t.Errorf("the limiter was called for an unlimited method: %v", r)
	default:
	}
}

func TestInterceptorAllowed(t *testing.T) {
	c := startFake(t, answer(allowed(), nil, nil))
	h := &spy{}

	resp, err := intercept(context.Background(), c, nil, h)

	if err != nil || resp != "ok" || !h.called {
		t.Fatalf("got %v, %v, called %v; want ok, nil, true", resp, err, h.called)
	}
}

func TestInterceptorDenied(t *testing.T) {
	c := startFake(t, answer(&ratelimiterv1.AllowResponse{Allowed: false, RetryAfter: durationpb.New(2 * time.Second)}, nil, nil))
	h := &spy{}

	_, err := intercept(context.Background(), c, nil, h)

	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Fatalf("code = %v (%v), want ResourceExhausted", got, err)
	}
	st, _ := status.FromError(err)
	if st.Message() != "rate limit exceeded" {
		t.Errorf("message = %q", st.Message())
	}
	var delay time.Duration
	for _, d := range st.Details() {
		if ri, ok := d.(*errdetails.RetryInfo); ok {
			delay = ri.GetRetryDelay().AsDuration()
		}
	}
	if delay != 2*time.Second {
		t.Errorf("RetryInfo delay = %v, want 2s", delay)
	}
	if h.called {
		t.Error("the handler ran for a denied request")
	}
}

func TestInterceptorLimiterFailures(t *testing.T) {
	tests := []struct {
		name      string
		limiter   codes.Code
		mode      FailMode
		wantRun   bool       // does the handler run?
		wantCode  codes.Code // what the caller gets when it does not
		wantLevel string     // log level expected, "" for none
	}{
		{"unavailable, fail open", codes.Unavailable, FailOpen, true, codes.OK, "level=WARN"},
		{"unavailable, fail closed", codes.Unavailable, FailClosed, false, codes.Unavailable, ""},
		{"deadline, fail open", codes.DeadlineExceeded, FailOpen, true, codes.OK, "level=WARN"},
		{"deadline, fail closed", codes.DeadlineExceeded, FailClosed, false, codes.Unavailable, ""},
		{"invalid argument, fail open", codes.InvalidArgument, FailOpen, true, codes.OK, "level=ERROR"},
		{"invalid argument, fail closed", codes.InvalidArgument, FailClosed, false, codes.Unavailable, "level=ERROR"},
		{"not found, fail open", codes.NotFound, FailOpen, true, codes.OK, "level=ERROR"},
		{"not found, fail closed", codes.NotFound, FailClosed, false, codes.Unavailable, "level=ERROR"},
		{"internal, fail open", codes.Internal, FailOpen, true, codes.OK, "level=ERROR"},
		{"internal, fail closed", codes.Internal, FailClosed, false, codes.Unavailable, "level=ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLogs(t)
			c := startFake(t, answer(nil, status.Error(tt.limiter, "limiter said so"), nil), WithFailMode(tt.mode))
			h := &spy{}

			_, err := intercept(context.Background(), c, nil, h)

			if h.called != tt.wantRun {
				t.Errorf("handler ran = %v, want %v", h.called, tt.wantRun)
			}
			if got := status.Code(err); got != tt.wantCode {
				t.Errorf("code = %v (%v), want %v", got, err, tt.wantCode)
			}
			if tt.wantLevel == "" && logs.Len() != 0 {
				t.Errorf("unexpected log: %s", logs)
			}
			if tt.wantLevel != "" && !strings.Contains(logs.String(), tt.wantLevel) {
				t.Errorf("log missing %q: %s", tt.wantLevel, logs)
			}
			if !tt.wantRun && strings.Contains(err.Error(), "limiter said so") {
				t.Errorf("the limiter's own message reached the caller: %v", err)
			}
		})
	}
}

// The caller's own deadline or cancellation is not the limiter's failure.
func TestInterceptorCallerGaveUp(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		want codes.Code
	}{
		{"canceled", canceled, codes.Canceled},
		{"deadline passed", expired, codes.DeadlineExceeded},
	}
	for _, tt := range tests {
		for _, mode := range []FailMode{FailOpen, FailClosed} {
			t.Run(tt.name, func(t *testing.T) {
				logs := captureLogs(t)
				c := startFake(t, answer(allowed(), nil, nil), WithFailMode(mode))
				h := &spy{}

				_, err := intercept(tt.ctx, c, nil, h)

				if got := status.Code(err); got != tt.want {
					t.Errorf("code = %v (%v), want %v", got, err, tt.want)
				}
				if h.called {
					t.Error("the handler ran for a caller that is gone")
				}
				if logs.Len() != 0 {
					t.Errorf("the fail mode was applied: %s", logs)
				}
			})
		}
	}
}

// The limiter call may take only as long as what is left of the incoming
// deadline, even if the Client's own timeout is longer.
func TestInterceptorShorterIncomingDeadlineWins(t *testing.T) {
	left := make(chan time.Duration, 1)
	c := startFake(t, func(ctx context.Context, _ *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error) {
		d, _ := ctx.Deadline()
		left <- time.Until(d)
		return allowed(), nil
	}, WithTimeout(time.Minute))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	if _, err := intercept(ctx, c, nil, &spy{}); err != nil {
		t.Fatalf("intercept: %v", err)
	}

	if got := <-left; got <= 0 || got > 300*time.Millisecond {
		t.Errorf("the limiter saw %v left, want at most the incoming 300ms and not the Client's minute", got)
	}
}

func TestInterceptorSlowLimiterFailsWithinTheTimeout(t *testing.T) {
	logs := captureLogs(t)
	c := startFake(t, func(ctx context.Context, _ *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, WithTimeout(20*time.Millisecond), WithFailMode(FailOpen))
	h := &spy{}

	start := time.Now()
	_, err := intercept(context.Background(), c, nil, h)

	if err != nil || !h.called {
		t.Fatalf("err = %v, handler ran = %v; want the handler to run (fail open)", err, h.called)
	}
	if took := time.Since(start); took > 5*time.Second { // a guard against a hang, not a measurement
		t.Errorf("took %v: the timeout did not apply", took)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("no warning logged: %s", logs)
	}
}

func TestInterceptorKey(t *testing.T) {
	withMeta := func(kv ...string) context.Context {
		return metadata.NewIncomingContext(
			peer.NewContext(context.Background(), &peer.Peer{Addr: remoteAddr("192.0.2.9:4000")}),
			metadata.Pairs(kv...))
	}
	tests := []struct {
		name string
		ctx  context.Context
		key  KeyFunc
		want string
	}{
		{"the key from the extractor", withMeta("x-api-key", "abc"), KeyFromMetadata("x-api-key"), "abc"},
		{"extractor finds nothing: the peer IP", withMeta(), KeyFromMetadata("x-api-key"), "192.0.2.9"},
		{"no extractor: the peer IP", withMeta("x-api-key", "abc"), nil, "192.0.2.9"},
		{"nothing at all: one shared bucket", context.Background(), nil, anonymousKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen := make(chan *ratelimiterv1.AllowRequest, 1)
			c := startFake(t, answer(allowed(), nil, seen))

			if _, err := intercept(tt.ctx, c, tt.key, &spy{}); err != nil {
				t.Fatalf("intercept: %v", err)
			}

			r := <-seen
			if r.GetKey() != tt.want {
				t.Errorf("key = %q, want %q", r.GetKey(), tt.want)
			}
			if r.GetNamespace() != "orders-api" || r.GetPolicy() != "create-order" {
				t.Errorf("namespace/policy = %q/%q", r.GetNamespace(), r.GetPolicy())
			}
		})
	}
}

// The key can be a user id or an IP: it must not reach the logs.
func TestInterceptorNeverLogsTheKey(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "user-secret-123"))
	for _, code := range []codes.Code{codes.Unavailable, codes.InvalidArgument} {
		t.Run(code.String(), func(t *testing.T) {
			logs := captureLogs(t)
			c := startFake(t, answer(nil, status.Error(code, "x"), nil))

			_, _ = intercept(ctx, c, KeyFromMetadata("x-api-key"), &spy{})

			if logs.Len() == 0 {
				t.Fatal("nothing logged, the check proves nothing")
			}
			if strings.Contains(logs.String(), "user-secret-123") {
				t.Errorf("the log leaks the key: %s", logs)
			}
		})
	}
}

// A policy map the caller changes afterwards must not affect the interceptor.
func TestInterceptorCopiesThePolicyMap(t *testing.T) {
	seen := make(chan *ratelimiterv1.AllowRequest, 1)
	c := startFake(t, answer(allowed(), nil, seen))
	policies := map[string]string{testMethod: "create-order"}
	ic := c.UnaryServerInterceptor(policies, nil)

	delete(policies, testMethod)
	if _, err := ic(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: testMethod}, (&spy{}).handle); err != nil {
		t.Fatalf("intercept: %v", err)
	}

	select {
	case <-seen:
	default:
		t.Error("the method was dropped from the interceptor when the caller's map changed")
	}
}
