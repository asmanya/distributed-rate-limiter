package client

import (
	"math"
	"net/http"
	"strconv"
	"time"
)

// HTTPMiddleware limits the requests of the handler it wraps. The policy belongs
// to the wrapped handler, so each route can have its own:
//
//	mux.Handle("/orders", client.HTTPMiddleware("create-order", key)(ordersHandler))
//
// key says what is being limited; nil means the caller's IP address.
//
// A denied request gets 429 with a Retry-After header. That header is a whole
// number of seconds, rounded up and never below 1: "0.5" is not valid, and 0
// would tell the caller to retry at once.
//
// When the limiter gives no answer, the fail mode decides (see FailMode): with
// FailClosed the request gets 503. If the caller has already gone, because it
// disconnected or its deadline passed, nothing is written and the fail mode does
// not apply.
func (c *Client) HTTPMiddleware(policy string, key KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The key extractors read the headers and the remote address from the
			// context. The wrapped handler still gets the original request.
			ctx := withHTTPRequest(r.Context(), r)

			d, err := c.Allow(ctx, policy, resolveKey(ctx, key))
			if err != nil {
				if r.Context().Err() != nil {
					return // nobody is waiting for an answer
				}
				c.logFailure(ctx, r.Method, policy, err)
				if c.cfg.failMode == FailOpen {
					next.ServeHTTP(w, r)
					return
				}
				// Generic on purpose: what the limiter said is not for the caller.
				http.Error(w, "rate limiter unavailable", http.StatusServiceUnavailable)
				return
			}
			if !d.Allowed {
				// The header has to be set before http.Error writes the status.
				w.Header().Set("Retry-After", retryAfterSeconds(d.RetryAfter))
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// retryAfterSeconds is d as the whole seconds a Retry-After header needs:
// rounded up, and at least 1.
func retryAfterSeconds(d time.Duration) string {
	secs := int64(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return strconv.FormatInt(secs, 10)
}
