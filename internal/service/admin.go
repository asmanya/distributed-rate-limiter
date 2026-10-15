package service

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
	"github.com/asmanya/distributed-rate-limiter/internal/limiter"
	"github.com/asmanya/distributed-rate-limiter/internal/policy"
)

// Admin implements ratelimiterv1.AdminServiceServer. It is registered only when
// ADMIN_ENABLED=true: Reset bypasses rate limiting, so on a public deploy the
// service must not exist at all.
type Admin struct {
	ratelimiterv1.UnimplementedAdminServiceServer

	policies PolicySource
	limiters map[policy.Algorithm]limiter.Limiter
	now      func() time.Time
	log      *slog.Logger
}

// NewAdmin builds the admin service from the same parts as NewRateLimit.
func NewAdmin(policies PolicySource, limiters map[policy.Algorithm]limiter.Limiter, now func() time.Time, log *slog.Logger) *Admin {
	return &Admin{policies: policies, limiters: limiters, now: now, log: log}
}

// Reset clears the stored state of one key, so its next request starts fresh.
// Resetting a key that has no state is not an error.
func (a *Admin) Reset(ctx context.Context, req *ratelimiterv1.ResetRequest) (*ratelimiterv1.ResetResponse, error) {
	p, ok := a.policies.Get(req.GetPolicy())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "policy %q not found", req.GetPolicy())
	}
	l, ok := a.limiters[p.Algorithm]
	if !ok {
		a.log.Error("no limiter for algorithm", "algorithm", p.Algorithm, "policy", req.GetPolicy())
		return nil, status.Error(codes.Internal, "internal error")
	}

	// Reset needs the period to locate the sliding window, but no cost or limit
	// check; the limiter validates just the namespace, policy and key.
	err := l.Reset(ctx, limiter.Request{
		Namespace: req.GetNamespace(),
		Policy:    req.GetPolicy(),
		Key:       req.GetKey(),
		Limit:     p.Limit,
		Period:    p.Period,
		Now:       a.now(),
	})
	if err != nil {
		logFailure(ctx, a.log, err)
		return nil, statusFor(err)
	}

	// Info level: resetting a limit is a notable admin action. The key itself is
	// never logged at info (it can be a user id or IP).
	a.log.InfoContext(ctx, "limit reset", "namespace", req.GetNamespace(), "policy", req.GetPolicy())
	return &ratelimiterv1.ResetResponse{}, nil
}
