package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
)

// httpSpy is the protected handler: it records whether it ran, and whether the
// request it got is the original one.
type httpSpy struct {
	called   bool
	original bool
}

func (s *httpSpy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.called = true
	s.original = r.Context().Value(httpHeaderKey{}) == nil
	_, _ = w.Write([]byte("ok"))
}

func newRequest(remote string, headers ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/users/987/orders", nil)
	r.RemoteAddr = remote
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	return r
}

func serveHTTP(c *Client, key KeyFunc, r *http.Request) (*httptest.ResponseRecorder, *httpSpy) {
	next := &httpSpy{}
	rec := httptest.NewRecorder()
	c.HTTPMiddleware("create-order", key)(next).ServeHTTP(rec, r)
	return rec, next
}

func TestHTTPAllowed(t *testing.T) {
	c := startFake(t, answer(allowed(), nil, nil))

	rec, next := serveHTTP(c, nil, newRequest("192.0.2.9:4000"))

	if rec.Code != http.StatusOK || rec.Body.String() != "ok" || !next.called {
		t.Fatalf("status %d, body %q, called %v; want 200, ok, true", rec.Code, rec.Body, next.called)
	}
	if !next.original {
		t.Error("the wrapped handler got the middleware's context, not the original request")
	}
}

func TestHTTPDenied(t *testing.T) {
	c := startFake(t, answer(&ratelimiterv1.AllowResponse{Allowed: false, RetryAfter: durationpb.New(1200 * time.Millisecond)}, nil, nil))

	rec, next := serveHTTP(c, nil, newRequest("192.0.2.9:4000"))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	// Result() is the response as it was when the status was written. Header()
	// is the live map and would hide a header that was set too late.
	if got := rec.Result().Header.Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2 (1.2s rounded up)", got)
	}
	if !strings.Contains(rec.Body.String(), "rate limit exceeded") {
		t.Errorf("body = %q", rec.Body)
	}
	if next.called {
		t.Error("the handler ran for a denied request")
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{1200 * time.Millisecond, "2"},
		{500 * time.Millisecond, "1"},
		{time.Nanosecond, "1"},
		{0, "1"}, // 0 would tell the caller to retry at once
		{-time.Second, "1"},
		{3 * time.Second, "3"},
		{3*time.Second + time.Nanosecond, "4"},
		{61 * time.Second, "61"},
	}
	for _, tt := range tests {
		if got := retryAfterSeconds(tt.in); got != tt.want {
			t.Errorf("retryAfterSeconds(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHTTPLimiterFailures(t *testing.T) {
	tests := []struct {
		name      string
		limiter   codes.Code
		mode      FailMode
		wantRun   bool
		wantLevel string
	}{
		{"unavailable, fail open", codes.Unavailable, FailOpen, true, "level=WARN"},
		{"unavailable, fail closed", codes.Unavailable, FailClosed, false, ""},
		{"deadline, fail open", codes.DeadlineExceeded, FailOpen, true, "level=WARN"},
		{"deadline, fail closed", codes.DeadlineExceeded, FailClosed, false, ""},
		{"invalid argument, fail open", codes.InvalidArgument, FailOpen, true, "level=ERROR"},
		{"invalid argument, fail closed", codes.InvalidArgument, FailClosed, false, "level=ERROR"},
		{"not found, fail open", codes.NotFound, FailOpen, true, "level=ERROR"},
		{"not found, fail closed", codes.NotFound, FailClosed, false, "level=ERROR"},
		{"internal, fail open", codes.Internal, FailOpen, true, "level=ERROR"},
		{"internal, fail closed", codes.Internal, FailClosed, false, "level=ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLogs(t)
			c := startFake(t, answer(nil, status.Error(tt.limiter, "limiter said so"), nil), WithFailMode(tt.mode))

			rec, next := serveHTTP(c, nil, newRequest("192.0.2.9:4000"))

			if next.called != tt.wantRun {
				t.Errorf("handler ran = %v, want %v", next.called, tt.wantRun)
			}
			wantStatus := http.StatusServiceUnavailable
			if tt.wantRun {
				wantStatus = http.StatusOK
			}
			if rec.Code != wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, wantStatus)
			}
			if tt.wantLevel == "" && logs.Len() != 0 {
				t.Errorf("unexpected log: %s", logs)
			}
			if tt.wantLevel != "" && !strings.Contains(logs.String(), tt.wantLevel) {
				t.Errorf("log missing %q: %s", tt.wantLevel, logs)
			}
			if strings.Contains(rec.Body.String(), "limiter said so") {
				t.Errorf("the limiter's own message reached the caller: %q", rec.Body)
			}
		})
	}
}

// A caller that has gone has nobody to answer, and its going is not the
// limiter's failure.
func TestHTTPCallerGone(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for name, ctx := range map[string]context.Context{"canceled": canceled, "deadline passed": expired} {
		for _, mode := range []FailMode{FailOpen, FailClosed} {
			t.Run(name, func(t *testing.T) {
				logs := captureLogs(t)
				c := startFake(t, answer(allowed(), nil, nil), WithFailMode(mode))

				rec, next := serveHTTP(c, nil, newRequest("192.0.2.9:4000").WithContext(ctx))

				if next.called {
					t.Error("the handler ran for a caller that is gone")
				}
				if rec.Body.Len() != 0 || len(rec.Header()) != 0 {
					t.Errorf("something was written: %q %v", rec.Body, rec.Header())
				}
				if logs.Len() != 0 {
					t.Errorf("the fail mode was applied: %s", logs)
				}
			})
		}
	}
}

func TestHTTPKey(t *testing.T) {
	tests := []struct {
		name string
		req  *http.Request
		key  KeyFunc
		want string
	}{
		{"the key from the header", newRequest("192.0.2.9:4000", "X-Api-Key", "abc"), KeyFromHeader("X-Api-Key"), "abc"},
		{"header missing: the IP, without the port", newRequest("192.0.2.9:4000"), KeyFromHeader("X-Api-Key"), "192.0.2.9"},
		{"no extractor: the IP", newRequest("192.0.2.9:4000", "X-Api-Key", "abc"), nil, "192.0.2.9"},
		{"a key the service would refuse is hashed", newRequest("192.0.2.9:4000", "X-Api-Key", "{evil}"), KeyFromHeader("X-Api-Key"), safeKey("{evil}")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen := make(chan *ratelimiterv1.AllowRequest, 1)
			c := startFake(t, answer(allowed(), nil, seen))

			serveHTTP(c, tt.key, tt.req)

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

// The key and the URL path (which can hold a user or order id) must not reach
// the logs.
func TestHTTPNeverLogsTheKeyOrThePath(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.InvalidArgument} {
		t.Run(code.String(), func(t *testing.T) {
			logs := captureLogs(t)
			c := startFake(t, answer(nil, status.Error(code, "x"), nil))

			serveHTTP(c, KeyFromHeader("X-Api-Key"), newRequest("192.0.2.9:4000", "X-Api-Key", "user-secret-123"))

			if logs.Len() == 0 {
				t.Fatal("nothing logged, the check proves nothing")
			}
			for _, leak := range []string{"user-secret-123", "987", "/users"} {
				if strings.Contains(logs.String(), leak) {
					t.Errorf("the log leaks %q: %s", leak, logs)
				}
			}
		})
	}
}

func TestHTTPSlowLimiterFailsWithinTheTimeout(t *testing.T) {
	captureLogs(t)
	c := startFake(t, func(ctx context.Context, _ *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, WithTimeout(20*time.Millisecond), WithFailMode(FailOpen))

	start := time.Now()
	rec, next := serveHTTP(c, nil, newRequest("192.0.2.9:4000"))

	if rec.Code != http.StatusOK || !next.called {
		t.Fatalf("status %d, called %v; want 200, true (fail open)", rec.Code, next.called)
	}
	if took := time.Since(start); took > 5*time.Second { // a guard against a hang, not a measurement
		t.Errorf("took %v: the timeout did not apply", took)
	}
}
