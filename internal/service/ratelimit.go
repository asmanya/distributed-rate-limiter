// Package service implements the gRPC handlers. It translates between the proto
// types and the limiter, and between limiter errors and gRPC status codes; it
// contains no algorithm and no Redis command.
package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	ratelimiterv1 "github.com/asmanya/distributed-rate-limiter/gen/ratelimiter/v1"
	"github.com/asmanya/distributed-rate-limiter/internal/limiter"
	"github.com/asmanya/distributed-rate-limiter/internal/metrics"
	"github.com/asmanya/distributed-rate-limiter/internal/policy"
)

// PolicySource looks up a policy by name. Declared here, where it is used, so
// tests can supply a plain map and the real *policy.Store satisfies it.
type PolicySource interface {
	Get(name string) (policy.Policy, bool)
}

// RateLimit implements ratelimiterv1.RateLimitServiceServer.
type RateLimit struct {
	ratelimiterv1.UnimplementedRateLimitServiceServer

	policies PolicySource
	limiters map[policy.Algorithm]limiter.Limiter
	now      func() time.Time
	log      *slog.Logger
	metrics  *metrics.Metrics
}

// NewRateLimit builds the service. now is the only clock: the limiter never
// reads one, so tests pass a fixed function here. m receives the decision counts.
func NewRateLimit(policies PolicySource, limiters map[policy.Algorithm]limiter.Limiter, now func() time.Time, log *slog.Logger, m *metrics.Metrics) *RateLimit {
	return &RateLimit{policies: policies, limiters: limiters, now: now, log: log, metrics: m}
}

// Allow decides and, if allowed, consumes the request's cost.
func (s *RateLimit) Allow(ctx context.Context, req *ratelimiterv1.AllowRequest) (*ratelimiterv1.AllowResponse, error) {
	d, err := s.decide(ctx, req.GetNamespace(), req.GetPolicy(), req.GetKey(), req.GetCost(),
		func(l limiter.Limiter) func(context.Context, limiter.Request) (limiter.Decision, error) {
			return l.Allow
		})
	if err != nil {
		return nil, err
	}
	// Only a real decision is counted: errors and unknown policies returned
	// above, so the policy label is always a configured name, never user input.
	// Peek does not count, it consumes nothing.
	s.metrics.Decisions.WithLabelValues(req.GetPolicy(), resultLabel(d.Allowed)).Inc()
	return &ratelimiterv1.AllowResponse{
		Allowed:    d.Allowed,
		Limit:      d.Limit,
		Remaining:  d.Remaining,
		RetryAfter: durationpb.New(d.RetryAfter),
		ResetAfter: durationpb.New(d.ResetAfter),
	}, nil
}

// Peek returns the decision Allow would make right now, without consuming.
func (s *RateLimit) Peek(ctx context.Context, req *ratelimiterv1.PeekRequest) (*ratelimiterv1.PeekResponse, error) {
	d, err := s.decide(ctx, req.GetNamespace(), req.GetPolicy(), req.GetKey(), req.GetCost(),
		func(l limiter.Limiter) func(context.Context, limiter.Request) (limiter.Decision, error) {
			return l.Peek
		})
	if err != nil {
		return nil, err
	}
	return &ratelimiterv1.PeekResponse{
		Allowed:    d.Allowed,
		Limit:      d.Limit,
		Remaining:  d.Remaining,
		RetryAfter: durationpb.New(d.RetryAfter),
		ResetAfter: durationpb.New(d.ResetAfter),
	}, nil
}

func resultLabel(allowed bool) string {
	if allowed {
		return "allowed"
	}
	return "denied"
}

// decide is the flow shared by Allow and Peek; pick chooses which limiter method
// to call.
func (s *RateLimit) decide(
	ctx context.Context,
	namespace, policyName, key string,
	cost int64,
	pick func(limiter.Limiter) func(context.Context, limiter.Request) (limiter.Decision, error),
) (limiter.Decision, error) {
	p, ok := s.policies.Get(policyName)
	if !ok {
		return limiter.Decision{}, status.Errorf(codes.NotFound, "policy %q not found", policyName)
	}

	if cost < 0 {
		return limiter.Decision{}, status.Error(codes.InvalidArgument, "cost must not be negative")
	}
	if cost == 0 {
		cost = 1 // the contract: 0 means 1
	}

	l, ok := s.limiters[p.Algorithm]
	if !ok {
		// Validation at load time makes this impossible; if it ever happens it
		// is a wiring bug, so report Internal rather than panic.
		s.log.Error("no limiter for algorithm", "algorithm", p.Algorithm, "policy", policyName)
		return limiter.Decision{}, status.Error(codes.Internal, "internal error")
	}

	d, err := pick(l)(ctx, limiter.Request{
		Namespace: namespace,
		Policy:    policyName,
		Key:       key,
		Limit:     p.Limit,
		Period:    p.Period,
		Cost:      cost,
		Now:       s.now(),
	})
	if err != nil {
		logFailure(ctx, s.log, err)
		return limiter.Decision{}, statusFor(err)
	}
	return d, nil
}

// logFailure records what the client is not told. Client mistakes are logged at
// debug (they are not our problem); store failures and our own bugs at error.
func logFailure(ctx context.Context, log *slog.Logger, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		log.DebugContext(ctx, "request ended by caller", "error", err)
	case errors.Is(err, limiter.ErrStoreUnavailable):
		log.ErrorContext(ctx, "rate limit store failed", "error", err)
	case errors.Is(err, limiter.ErrInvalidKey), errors.Is(err, limiter.ErrInvalidName),
		errors.Is(err, limiter.ErrInvalidCost), errors.Is(err, limiter.ErrCostExceedsLimit):
		log.DebugContext(ctx, "invalid request", "error", err)
	default:
		log.ErrorContext(ctx, "unexpected limiter error", "error", err)
	}
}
