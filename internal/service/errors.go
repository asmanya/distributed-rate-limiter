package service

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/asmanya/distributed-rate-limiter/internal/limiter"
)

// statusFor maps an error from the limiter or policy lookup to the gRPC status
// the client sees. This table is the service's contract with its callers.
//
// A denial is not an error and never reaches here: it is an OK response with
// allowed=false. Errors mean "no decision could be made".
func statusFor(err error) error {
	switch {
	// The caller's own deadline or cancellation comes first: it is not the
	// store's fault, and reporting it as Unavailable would trip the SDK's
	// fail mode for the wrong reason.
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")

	case errors.Is(err, limiter.ErrStoreUnavailable):
		// Generic on purpose: the real error can hold the Redis host, port or a
		// URL with a password. It goes to the logs, never to the client.
		return status.Error(codes.Unavailable, "rate limit store unavailable")

	case errors.Is(err, limiter.ErrInvalidKey),
		errors.Is(err, limiter.ErrInvalidName),
		errors.Is(err, limiter.ErrInvalidCost),
		errors.Is(err, limiter.ErrCostExceedsLimit):
		return status.Error(codes.InvalidArgument, err.Error())

	default:
		// Includes a policy with a bad limit or period: validation at load time
		// should make that impossible, so it is our bug, not the caller's.
		return status.Error(codes.Internal, "internal error")
	}
}
