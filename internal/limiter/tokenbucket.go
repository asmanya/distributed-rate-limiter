package limiter

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// TokenBucket is the token bucket algorithm backed by Lua scripts.
type TokenBucket struct {
	rdb Client
}

var _ Limiter = (*TokenBucket)(nil)

// NewTokenBucket returns a TokenBucket that runs its scripts on rdb.
func NewTokenBucket(rdb Client) *TokenBucket {
	return &TokenBucket{rdb: rdb}
}

// Allow decides whether req may proceed and, if so, consumes req.Cost tokens.
func (t *TokenBucket) Allow(ctx context.Context, req Request) (Decision, error) {
	if err := req.Validate(); err != nil {
		return Decision{}, err
	}
	keys, args := tokenBucketCall(req)
	return decide(tokenBucketScript.Run(ctx, t.rdb, keys, args...), req.Limit)
}

// Peek returns the decision Allow would make right now, without changing
// anything. It runs the read-only script with EVALSHA_RO.
func (t *TokenBucket) Peek(ctx context.Context, req Request) (Decision, error) {
	if err := req.Validate(); err != nil {
		return Decision{}, err
	}
	keys, args := tokenBucketCall(req)
	return decide(tokenBucketPeekScript.RunRO(ctx, t.rdb, keys, args...), req.Limit)
}

// Reset deletes the bucket. Deleting a missing key is not an error.
func (t *TokenBucket) Reset(ctx context.Context, req Request) error {
	if err := req.validateTarget(); err != nil {
		return err
	}
	if err := t.rdb.Del(ctx, tokenBucketKey(req)).Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}
	return nil
}

// tokenBucketCall builds the KEYS and ARGV shared by Allow and Peek.
func tokenBucketCall(req Request) ([]string, []any) {
	return []string{tokenBucketKey(req)},
		[]any{req.Limit, formatRate(req), req.Cost, req.nowMillis()}
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
