package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
	"github.com/asmanya/distributed-rate-limiter/internal/limiter"
	"github.com/asmanya/distributed-rate-limiter/internal/policy"
)

func newAdmin(f *fakeLimiter, log *slog.Logger) *Admin {
	return NewAdmin(testPolicies(), limitersWith(f), testNow, log)
}

func baseReset() *ratelimiterv1.ResetRequest {
	return &ratelimiterv1.ResetRequest{Namespace: "orders-api", Policy: "search", Key: "user-1"}
}

func TestReset(t *testing.T) {
	f := &fakeLimiter{}
	resp, err := newAdmin(f, discardLogger()).Reset(context.Background(), baseReset())
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	if len(f.reset) != 1 {
		t.Fatalf("limiter Reset called %d times, want 1", len(f.reset))
	}
	want := limiter.Request{
		Namespace: "orders-api", Policy: "search", Key: "user-1",
		Limit: 10, Period: time.Minute, Now: testNow(),
	}
	if f.reset[0] != want {
		t.Fatalf("limiter saw %+v, want %+v", f.reset[0], want)
	}
}

func TestResetErrorMapping(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*ratelimiterv1.ResetRequest)
		limiterErr  error
		wantCode    codes.Code
		wantReached bool
	}{
		{"unknown policy", func(r *ratelimiterv1.ResetRequest) { r.Policy = "nope" }, nil, codes.NotFound, false},
		{"empty key", func(r *ratelimiterv1.ResetRequest) { r.Key = "" }, limiter.ErrInvalidKey, codes.InvalidArgument, true},
		{"store unavailable", func(*ratelimiterv1.ResetRequest) {},
			fmt.Errorf("%w: %w", limiter.ErrStoreUnavailable, errors.New("down")), codes.Unavailable, true},
		{"caller deadline", func(*ratelimiterv1.ResetRequest) {},
			fmt.Errorf("%w: %w", limiter.ErrStoreUnavailable, context.DeadlineExceeded), codes.DeadlineExceeded, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLimiter{err: tt.limiterErr}
			req := baseReset()
			tt.mutate(req)

			_, err := newAdmin(f, discardLogger()).Reset(context.Background(), req)

			if got := status.Code(err); got != tt.wantCode {
				t.Fatalf("code = %v (%v), want %v", got, err, tt.wantCode)
			}
			if reached := len(f.reset) == 1; reached != tt.wantReached {
				t.Errorf("limiter calls = %d, wantReached %v", len(f.reset), tt.wantReached)
			}
		})
	}
}

func TestResetNoLimiterForAlgorithm(t *testing.T) {
	a := NewAdmin(testPolicies(), map[policy.Algorithm]limiter.Limiter{}, testNow, discardLogger())
	_, err := a.Reset(context.Background(), baseReset())
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("code = %v, want Internal", got)
	}
}

// The key can be a user id or an IP, so it must not appear in info logs.
func TestResetNeverLogsTheKey(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	req := baseReset()
	req.Key = "user-secret-123"
	if _, err := newAdmin(&fakeLimiter{}, log).Reset(context.Background(), req); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "limit reset") {
		t.Errorf("no audit line for a reset: %q", out)
	}
	if strings.Contains(out, "user-secret-123") {
		t.Errorf("info log leaks the key: %q", out)
	}
}
