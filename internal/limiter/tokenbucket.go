package limiter

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// TokenBucket is the token bucket algorithm backed by one Lua script.
type TokenBucket struct {
	rdb redis.Scripter
}

// NewTokenBucket returns a TokenBucket that runs its script on rdb. It takes
// the Scripter interface, not a concrete client, so single-node and cluster
// clients both work.
func NewTokenBucket(rdb redis.Scripter) *TokenBucket {
	return &TokenBucket{rdb: rdb}
}

// Allow decides whether req may proceed and, if so, consumes req.Cost tokens.
func (t *TokenBucket) Allow(ctx context.Context, req Request) (Decision, error) {
	if err := req.Validate(); err != nil {
		return Decision{}, err
	}

	cmd := tokenBucketScript.Run(ctx, t.rdb,
		[]string{tokenBucketKey(req)},
		req.Limit, formatRate(req), req.Cost, req.nowMillis(),
	)
	if err := cmd.Err(); err != nil {
		// Both errors stay in the chain: callers match ErrStoreUnavailable, and
		// can still tell a caller's expired deadline from a Redis failure.
		return Decision{}, fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}

	reply, err := cmd.Int64Slice()
	if err != nil {
		// Redis answered, but not with integers: a script bug, not an outage.
		return Decision{}, fmt.Errorf("limiter: unexpected script reply: %w", err)
	}
	return decisionFromReply(req.Limit, reply)
}

// formatRate returns tokens per millisecond as the shortest string that
// parses back to the exact same float. Fixed decimals would turn 0.001 into
// 0.00 and the bucket would never refill.
func formatRate(req Request) string {
	rate := float64(req.Limit) / float64(req.Period.Milliseconds())
	return strconv.FormatFloat(rate, 'g', -1, 64)
}

// decisionFromReply turns the script's four integers into a Decision.
func decisionFromReply(limit int64, reply []int64) (Decision, error) {
	if len(reply) != 4 {
		return Decision{}, fmt.Errorf("limiter: script returned %d values, want 4", len(reply))
	}
	return Decision{
		Allowed:    reply[0] == 1,
		Limit:      limit,
		Remaining:  reply[1],
		RetryAfter: time.Duration(reply[2]) * time.Millisecond,
		ResetAfter: time.Duration(reply[3]) * time.Millisecond,
	}, nil
}
