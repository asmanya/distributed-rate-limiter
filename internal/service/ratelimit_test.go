package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
	"github.com/asmanya/distributed-rate-limiter/internal/limiter"
	"github.com/asmanya/distributed-rate-limiter/internal/policy"
)

// fakeLimiter returns a configured result and records every request it sees, so
// tests can check what the service passed down. Single-goroutine use only.
type fakeLimiter struct {
	decision limiter.Decision
	err      error

	allowed []limiter.Request
	peeked  []limiter.Request
	reset   []limiter.Request
}

var _ limiter.Limiter = (*fakeLimiter)(nil)

func (f *fakeLimiter) Allow(_ context.Context, req limiter.Request) (limiter.Decision, error) {
	f.allowed = append(f.allowed, req)
	return f.decision, f.err
}

func (f *fakeLimiter) Peek(_ context.Context, req limiter.Request) (limiter.Decision, error) {
	f.peeked = append(f.peeked, req)
	return f.decision, f.err
}

func (f *fakeLimiter) Reset(_ context.Context, req limiter.Request) error {
	f.reset = append(f.reset, req)
	return f.err
}

// mapPolicies is an in-memory PolicySource: no file, no store.
type mapPolicies map[string]policy.Policy

func (m mapPolicies) Get(name string) (policy.Policy, bool) {
	p, ok := m[name]
	return p, ok
}

func testPolicies() mapPolicies {
	return mapPolicies{
		"search": {Name: "search", Algorithm: policy.TokenBucket, Limit: 10, Period: time.Minute},
	}
}

// testNow is the injected clock: time is fixed, nothing sleeps.
func testNow() time.Time { return time.UnixMilli(1_700_000_000_000) }

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func limitersWith(f limiter.Limiter) map[policy.Algorithm]limiter.Limiter {
	return map[policy.Algorithm]limiter.Limiter{policy.TokenBucket: f}
}

func newRateLimit(f *fakeLimiter) *RateLimit {
	return NewRateLimit(testPolicies(), limitersWith(f), testNow, discardLogger())
}

func baseAllow() *ratelimiterv1.AllowRequest {
	return &ratelimiterv1.AllowRequest{Namespace: "orders-api", Policy: "search", Key: "user-1", Cost: 1}
}

func TestAllowResponses(t *testing.T) {
	tests := []struct {
		name     string
		decision limiter.Decision
		want     *ratelimiterv1.AllowResponse
	}{
		{
			"allowed",
			limiter.Decision{Allowed: true, Limit: 10, Remaining: 9, ResetAfter: 6 * time.Second},
			&ratelimiterv1.AllowResponse{
				Allowed: true, Limit: 10, Remaining: 9,
				RetryAfter: durationpb.New(0), ResetAfter: durationpb.New(6 * time.Second),
			},
		},
		{
			// A denial is a normal OK response, not an error.
			"denied",
			limiter.Decision{Allowed: false, Limit: 10, Remaining: 0, RetryAfter: 2 * time.Second, ResetAfter: 10 * time.Second},
			&ratelimiterv1.AllowResponse{
				Allowed: false, Limit: 10, Remaining: 0,
				RetryAfter: durationpb.New(2 * time.Second), ResetAfter: durationpb.New(10 * time.Second),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLimiter{decision: tt.decision}
			got, err := newRateLimit(f).Allow(context.Background(), baseAllow())
			if err != nil {
				t.Fatalf("Allow: %v", err)
			}
			if !proto.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// This table is the error mapping spec: one row per line of the mapping.
func TestAllowErrorMapping(t *testing.T) {
	storeDown := fmt.Errorf("%w: %w", limiter.ErrStoreUnavailable, errors.New("dial tcp 10.0.0.5:6379: refused"))

	tests := []struct {
		name        string
		mutate      func(*ratelimiterv1.AllowRequest)
		limiterErr  error
		wantCode    codes.Code
		wantReached bool // did the request reach the limiter?
	}{
		{"unknown policy", func(r *ratelimiterv1.AllowRequest) { r.Policy = "nope" }, nil, codes.NotFound, false},
		{"negative cost", func(r *ratelimiterv1.AllowRequest) { r.Cost = -1 }, nil, codes.InvalidArgument, false},
		{"empty key", func(r *ratelimiterv1.AllowRequest) { r.Key = "" }, limiter.ErrInvalidKey, codes.InvalidArgument, true},
		{"invalid namespace", func(r *ratelimiterv1.AllowRequest) { r.Namespace = "a:b" }, limiter.ErrInvalidName, codes.InvalidArgument, true},
		{"cost above limit", func(r *ratelimiterv1.AllowRequest) { r.Cost = 99 }, limiter.ErrCostExceedsLimit, codes.InvalidArgument, true},
		{"store unavailable", func(*ratelimiterv1.AllowRequest) {}, storeDown, codes.Unavailable, true},
		{"caller deadline", func(*ratelimiterv1.AllowRequest) {},
			fmt.Errorf("%w: %w", limiter.ErrStoreUnavailable, context.DeadlineExceeded), codes.DeadlineExceeded, true},
		{"caller canceled", func(*ratelimiterv1.AllowRequest) {},
			fmt.Errorf("%w: %w", limiter.ErrStoreUnavailable, context.Canceled), codes.Canceled, true},
		{"bad policy params", func(*ratelimiterv1.AllowRequest) {}, limiter.ErrInvalidPolicy, codes.Internal, true},
		{"unexpected error", func(*ratelimiterv1.AllowRequest) {}, errors.New("boom"), codes.Internal, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLimiter{err: tt.limiterErr}
			req := baseAllow()
			tt.mutate(req)

			resp, err := newRateLimit(f).Allow(context.Background(), req)

			if resp != nil {
				t.Errorf("got a response %v alongside an error", resp)
			}
			if got := status.Code(err); got != tt.wantCode {
				t.Fatalf("code = %v (%v), want %v", got, err, tt.wantCode)
			}
			if reached := len(f.allowed) == 1; reached != tt.wantReached {
				t.Errorf("limiter calls = %d, wantReached %v", len(f.allowed), tt.wantReached)
			}
		})
	}
}

func TestStoreErrorIsNotLeaked(t *testing.T) {
	f := &fakeLimiter{err: fmt.Errorf("%w: %w", limiter.ErrStoreUnavailable,
		errors.New("dial tcp 10.0.0.5:6379: password=hunter2"))}

	_, err := newRateLimit(f).Allow(context.Background(), baseAllow())

	msg := status.Convert(err).Message()
	if msg != "rate limit store unavailable" {
		t.Errorf("message = %q, want the generic message", msg)
	}
	for _, secret := range []string{"hunter2", "10.0.0.5", "6379"} {
		if strings.Contains(msg, secret) {
			t.Errorf("message leaks %q: %s", secret, msg)
		}
	}
}

func TestAllowCost(t *testing.T) {
	tests := []struct {
		name     string
		cost     int64
		wantCost int64
	}{
		{"zero means one", 0, 1},
		{"one stays one", 1, 1},
		{"three stays three", 3, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLimiter{decision: limiter.Decision{Allowed: true}}
			req := baseAllow()
			req.Cost = tt.cost
			if _, err := newRateLimit(f).Allow(context.Background(), req); err != nil {
				t.Fatalf("Allow: %v", err)
			}
			if len(f.allowed) != 1 || f.allowed[0].Cost != tt.wantCost {
				t.Fatalf("limiter saw %+v, want cost %d", f.allowed, tt.wantCost)
			}
		})
	}
}

func TestAllowPassesPolicyAndClock(t *testing.T) {
	f := &fakeLimiter{decision: limiter.Decision{Allowed: true}}
	if _, err := newRateLimit(f).Allow(context.Background(), baseAllow()); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	want := limiter.Request{
		Namespace: "orders-api",
		Policy:    "search",
		Key:       "user-1",
		Limit:     10,          // from the policy, not the request
		Period:    time.Minute, // from the policy
		Cost:      1,
		Now:       testNow(), // from the injected clock
	}
	if len(f.allowed) != 1 || f.allowed[0] != want {
		t.Fatalf("limiter saw %+v, want %+v", f.allowed, want)
	}
}

func TestNoLimiterForAlgorithm(t *testing.T) {
	// The policy asks for an algorithm no limiter was wired for: a wiring bug,
	// reported as Internal, never a panic.
	s := NewRateLimit(testPolicies(), map[policy.Algorithm]limiter.Limiter{}, testNow, discardLogger())
	_, err := s.Allow(context.Background(), baseAllow())
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("code = %v, want Internal", got)
	}
}

func TestPeek(t *testing.T) {
	t.Run("uses peek, never allow", func(t *testing.T) {
		f := &fakeLimiter{decision: limiter.Decision{Allowed: true, Limit: 10, Remaining: 9}}
		req := &ratelimiterv1.PeekRequest{Namespace: "orders-api", Policy: "search", Key: "user-1"}

		got, err := newRateLimit(f).Peek(context.Background(), req)
		if err != nil {
			t.Fatalf("Peek: %v", err)
		}
		if !got.GetAllowed() || got.GetRemaining() != 9 {
			t.Errorf("got %v", got)
		}
		if len(f.peeked) != 1 || len(f.allowed) != 0 {
			t.Fatalf("peek calls = %d, allow calls = %d, want 1 and 0", len(f.peeked), len(f.allowed))
		}
		if f.peeked[0].Cost != 1 {
			t.Errorf("cost = %d, want 1 (0 means 1)", f.peeked[0].Cost)
		}
	})

	t.Run("unknown policy", func(t *testing.T) {
		req := &ratelimiterv1.PeekRequest{Namespace: "orders-api", Policy: "nope", Key: "k"}
		_, err := newRateLimit(&fakeLimiter{}).Peek(context.Background(), req)
		if got := status.Code(err); got != codes.NotFound {
			t.Fatalf("code = %v, want NotFound", got)
		}
	})

	t.Run("store unavailable", func(t *testing.T) {
		f := &fakeLimiter{err: fmt.Errorf("%w: %w", limiter.ErrStoreUnavailable, errors.New("down"))}
		req := &ratelimiterv1.PeekRequest{Namespace: "orders-api", Policy: "search", Key: "k"}
		_, err := newRateLimit(f).Peek(context.Background(), req)
		if got := status.Code(err); got != codes.Unavailable {
			t.Fatalf("code = %v, want Unavailable", got)
		}
	})
}
