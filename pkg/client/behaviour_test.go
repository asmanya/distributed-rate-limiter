package client

// The SDK's outcome table, and where each case is tested:
//
//	allowed ................................ TestInterceptorAllowed, TestHTTPAllowed, TestOutcomeMatrix
//	denied (ResourceExhausted / 429) ........ TestInterceptorDenied, TestHTTPDenied, TestOutcomeMatrix,
//	                                          TestInterceptorOverTheWire
//	limiter down, fail open (handler + warn)  TestInterceptorLimiterFailures, TestHTTPLimiterFailures,
//	                                          TestOutcomeMatrix, TestInterceptorOverTheWire
//	limiter down, fail closed (Unavailable / 503)
//	                                          the same tests
//	slow limiter (fail mode within the timeout)
//	                                          TestInterceptorSlowLimiterFailsWithinTheTimeout,
//	                                          TestHTTPSlowLimiterFailsWithinTheTimeout, TestSlowLimiterLatency
//	method not in the map .................. TestInterceptorMethodNotInMap
//	key missing, peer fallback ............. TestKeyFromPeer, TestResolveKeyFallback,
//	                                          TestInterceptorKey, TestHTTPKey
//	InvalidArgument / NotFound (misconfig) . the LimiterFailures tests and TestOutcomeMatrix
//	the caller gave up ..................... TestInterceptorCallerGaveUp, TestHTTPCallerGone

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
)

func deniedResp(retry time.Duration) *ratelimiterv1.AllowResponse {
	return &ratelimiterv1.AllowResponse{Allowed: false, RetryAfter: durationpb.New(retry)}
}

// TestOutcomeMatrix runs every row of the outcome table through both transports.
// The gRPC interceptor and the HTTP middleware must reach the same decision for
// the same limiter answer: if one of them changes, this is the test that says so.
func TestOutcomeMatrix(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "x")
	deadline := status.Error(codes.DeadlineExceeded, "x")
	invalid := status.Error(codes.InvalidArgument, "x")
	notFound := status.Error(codes.NotFound, "x")

	tests := []struct {
		name      string
		resp      *ratelimiterv1.AllowResponse
		err       error
		mode      FailMode
		wantRun   bool       // does the protected handler run?
		wantGRPC  codes.Code // what the caller gets when it does not
		wantHTTP  int
		wantLevel string // "" means nothing is logged
	}{
		{"allowed", allowed(), nil, FailOpen, true, codes.OK, http.StatusOK, ""},
		{"denied", deniedResp(2 * time.Second), nil, FailOpen, false, codes.ResourceExhausted, http.StatusTooManyRequests, ""},
		{"denied, fail closed", deniedResp(2 * time.Second), nil, FailClosed, false, codes.ResourceExhausted, http.StatusTooManyRequests, ""},
		{"unavailable, fail open", nil, unavailable, FailOpen, true, codes.OK, http.StatusOK, "level=WARN"},
		{"unavailable, fail closed", nil, unavailable, FailClosed, false, codes.Unavailable, http.StatusServiceUnavailable, ""},
		{"deadline, fail open", nil, deadline, FailOpen, true, codes.OK, http.StatusOK, "level=WARN"},
		{"deadline, fail closed", nil, deadline, FailClosed, false, codes.Unavailable, http.StatusServiceUnavailable, ""},
		{"invalid argument, fail open", nil, invalid, FailOpen, true, codes.OK, http.StatusOK, "level=ERROR"},
		{"invalid argument, fail closed", nil, invalid, FailClosed, false, codes.Unavailable, http.StatusServiceUnavailable, "level=ERROR"},
		{"not found, fail open", nil, notFound, FailOpen, true, codes.OK, http.StatusOK, "level=ERROR"},
		{"not found, fail closed", nil, notFound, FailClosed, false, codes.Unavailable, http.StatusServiceUnavailable, "level=ERROR"},
	}
	for _, tt := range tests {
		check := func(t *testing.T, ran bool, logs string) {
			t.Helper()
			if ran != tt.wantRun {
				t.Errorf("handler ran = %v, want %v", ran, tt.wantRun)
			}
			if tt.wantLevel == "" && logs != "" {
				t.Errorf("unexpected log: %s", logs)
			}
			if tt.wantLevel != "" && !strings.Contains(logs, tt.wantLevel) {
				t.Errorf("log missing %q: %s", tt.wantLevel, logs)
			}
		}

		t.Run(tt.name+"/grpc", func(t *testing.T) {
			logs := captureLogs(t)
			c := startFake(t, answer(tt.resp, tt.err, nil), WithFailMode(tt.mode))
			h := &spy{}

			_, err := intercept(context.Background(), c, nil, h)

			check(t, h.called, logs.String())
			if got := status.Code(err); got != tt.wantGRPC {
				t.Errorf("code = %v (%v), want %v", got, err, tt.wantGRPC)
			}
		})

		t.Run(tt.name+"/http", func(t *testing.T) {
			logs := captureLogs(t)
			c := startFake(t, answer(tt.resp, tt.err, nil), WithFailMode(tt.mode))

			rec, next := serveHTTP(c, nil, newRequest("192.0.2.9:4000"))

			check(t, next.called, logs.String())
			if rec.Code != tt.wantHTTP {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantHTTP)
			}
		})
	}
}

// startProtected runs a real gRPC server that has the interceptor installed and
// returns a client for it. The protected service is gRPC's own health service,
// so no proto of our own is needed; its Check method is the limited one.
func startProtected(t *testing.T, c *Client) healthpb.HealthClient {
	t.Helper()
	policies := map[string]string{"/grpc.health.v1.Health/Check": "health"}
	srv := grpc.NewServer(grpc.UnaryInterceptor(c.UnaryServerInterceptor(policies, nil)))
	healthpb.RegisterHealthServer(srv, health.NewServer())

	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return healthpb.NewHealthClient(conn)
}

// TestInterceptorOverTheWire installs the interceptor on a real server and calls
// it from a real client, so the status and its RetryInfo detail have to survive
// being serialised, not just being built.
func TestInterceptorOverTheWire(t *testing.T) {
	tests := []struct {
		name       string
		resp       *ratelimiterv1.AllowResponse
		err        error
		mode       FailMode
		wantCode   codes.Code
		wantDetail time.Duration // the RetryInfo delay, 0 when there is none
	}{
		{"allowed", allowed(), nil, FailOpen, codes.OK, 0},
		{"denied", deniedResp(2 * time.Second), nil, FailOpen, codes.ResourceExhausted, 2 * time.Second},
		{"limiter down, fail closed", nil, status.Error(codes.Unavailable, "x"), FailClosed, codes.Unavailable, 0},
		{"limiter down, fail open", nil, status.Error(codes.Unavailable, "x"), FailOpen, codes.OK, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captureLogs(t)
			c := startFake(t, answer(tt.resp, tt.err, nil), WithFailMode(tt.mode))
			protected := startProtected(t, c)

			resp, err := protected.Check(context.Background(), &healthpb.HealthCheckRequest{})

			if got := status.Code(err); got != tt.wantCode {
				t.Fatalf("code = %v (%v), want %v", got, err, tt.wantCode)
			}
			if tt.wantCode == codes.OK {
				if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
					t.Errorf("status = %v, want SERVING", resp.GetStatus())
				}
				return
			}
			var delay time.Duration
			if st, ok := status.FromError(err); ok {
				for _, d := range st.Details() {
					if ri, ok := d.(*errdetails.RetryInfo); ok {
						delay = ri.GetRetryDelay().AsDuration()
					}
				}
			}
			if delay != tt.wantDetail {
				t.Errorf("RetryInfo delay on the client = %v, want %v", delay, tt.wantDetail)
			}
		})
	}
}

// TestSlowLimiterLatency pins "the fail mode applies within about the timeout".
// A limiter that never answers must not hold the request for less than the
// timeout (it cannot) nor for far more (the timeout did not apply). The upper
// bound is generous on purpose: a tight one would fail on a loaded machine.
func TestSlowLimiterLatency(t *testing.T) {
	const timeout = 20 * time.Millisecond
	newSlow := func(t *testing.T) *Client {
		t.Helper()
		captureLogs(t)
		return startFake(t, func(ctx context.Context, _ *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, WithTimeout(timeout), WithFailMode(FailOpen))
	}
	within := func(t *testing.T, took time.Duration) {
		t.Helper()
		if took < timeout {
			t.Errorf("took %v, less than the %v timeout", took, timeout)
		}
		if took >= time.Second {
			t.Errorf("took %v: the %v timeout did not apply", took, timeout)
		}
	}

	t.Run("grpc", func(t *testing.T) {
		c := newSlow(t)
		h := &spy{}
		start := time.Now()
		_, err := intercept(context.Background(), c, nil, h)
		within(t, time.Since(start))
		if err != nil || !h.called {
			t.Errorf("err = %v, handler ran = %v; want the handler to run (fail open)", err, h.called)
		}
	})
	t.Run("http", func(t *testing.T) {
		c := newSlow(t)
		start := time.Now()
		rec, next := serveHTTP(c, nil, newRequest("192.0.2.9:4000"))
		within(t, time.Since(start))
		if rec.Code != http.StatusOK || !next.called {
			t.Errorf("status %d, handler ran = %v; want 200, true (fail open)", rec.Code, next.called)
		}
	})
}
