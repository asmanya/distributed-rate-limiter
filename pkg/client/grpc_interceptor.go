package client

import (
	"context"
	"log/slog"
	"maps"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// UnaryServerInterceptor limits the unary RPCs of the service it is installed
// on. Streaming RPCs are not limited.
//
// policies maps a full method name ("/pkg.Service/Method") to the policy to
// apply; a method that is not in the map is not limited. key says what is being
// limited; nil means the caller's IP address.
//
// A denied request gets codes.ResourceExhausted with a RetryInfo detail: that is
// the standard Google error model, which many client libraries read to back off
// by themselves, and a custom header is read by none.
//
// When the limiter gives no answer, the fail mode decides (see FailMode). The
// caller's own deadline or cancellation is not the limiter's failure: it is
// returned as it is and the fail mode does not apply.
func (c *Client) UnaryServerInterceptor(policies map[string]string, key KeyFunc) grpc.UnaryServerInterceptor {
	policies = maps.Clone(policies) // the caller may change its map later
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		policy, ok := policies[info.FullMethod]
		if !ok {
			return handler(ctx, req)
		}

		// The timeout of the call is the shorter of what is left of the incoming
		// request's deadline and the Client's own: context.WithTimeout in Allow
		// keeps the earlier one.
		d, err := c.Allow(ctx, policy, resolveKey(ctx, key))
		if err != nil {
			if ctx.Err() != nil {
				// The caller gave up; running the handler or failing closed
				// would answer someone who is no longer there.
				return nil, status.FromContextError(ctx.Err()).Err()
			}
			return c.failed(ctx, info.FullMethod, policy, err, req, handler)
		}
		if !d.Allowed {
			return nil, denied(d.RetryAfter)
		}
		return handler(ctx, req)
	}
}

// failed applies the fail mode to a limiter error.
func (c *Client) failed(ctx context.Context, method, policy string, err error, req any, handler grpc.UnaryHandler) (any, error) {
	c.logFailure(ctx, method, policy, err)

	if c.cfg.failMode == FailOpen {
		return handler(ctx, req)
	}
	// Generic on purpose: what the limiter said is not for the caller.
	return nil, status.Error(codes.Unavailable, "rate limiter unavailable")
}

// logFailure records why the limiter gave no answer. It is shared by the
// interceptor and the middleware, so both log the same way. where is the method
// or the HTTP verb: never the URL path or the request key, which can be a user
// id or an IP.
func (c *Client) logFailure(ctx context.Context, where, policy string, err error) {
	code := status.Code(err)
	log := slog.Default()

	switch code {
	case codes.Unavailable, codes.DeadlineExceeded:
		// The limiter is down or slow. Under FailClosed the caller sees the
		// rejection itself, so only FailOpen needs a line saying limits are off.
		if c.cfg.failMode == FailOpen {
			log.WarnContext(ctx, "rate limiter unavailable, failing open",
				"method", where, "policy", policy, "code", code.String())
		}
	default:
		// InvalidArgument and NotFound mean the SDK is set up wrongly (a policy
		// name with a typo is a bug, not an outage), and anything else is a
		// surprise. The fail mode still applies, but loudly: under FailClosed a
		// typo would take the whole service down, and nobody should miss it.
		log.ErrorContext(ctx, "rate limiter returned an error, applying the fail mode",
			"method", where, "policy", policy, "code", code.String(), "error", err)
	}
}

// denied is the error for a request over its limit.
func denied(retryAfter time.Duration) error {
	st := status.New(codes.ResourceExhausted, "rate limit exceeded")
	withInfo, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(retryAfter)})
	if err != nil {
		return st.Err() // still a correct ResourceExhausted, only without the hint
	}
	return withInfo.Err()
}
