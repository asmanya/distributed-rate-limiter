package limiter

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Lua sources are embedded so the binary carries its scripts; there is no
// file path to get wrong at runtime.
//
//go:embed scripts/token_bucket.lua
var tokenBucketSrc string

// tokenBucketScript is created once and never changes. Run tries EVALSHA first
// and falls back to EVAL on NOSCRIPT, so the source is sent only when Redis
// does not know it yet.
var tokenBucketScript = redis.NewScript(tokenBucketSrc)

//go:embed scripts/sliding_window.lua
var slidingWindowSrc string

// slidingWindowScript is created once and never changes, like tokenBucketScript.
var slidingWindowScript = redis.NewScript(slidingWindowSrc)

//go:embed scripts/token_bucket_peek.lua
var tokenBucketPeekSrc string

// tokenBucketPeekScript is the read-only twin of tokenBucketScript.
var tokenBucketPeekScript = redis.NewScript(tokenBucketPeekSrc)

//go:embed scripts/sliding_window_peek.lua
var slidingWindowPeekSrc string

// slidingWindowPeekScript is the read-only twin of slidingWindowScript.
var slidingWindowPeekScript = redis.NewScript(slidingWindowPeekSrc)

// Client is what the limiters need from Redis: run scripts and delete keys.
// Both *redis.Client and *redis.ClusterClient satisfy it.
type Client interface {
	redis.Scripter
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// decide turns a finished script call into a Decision. A Redis failure becomes
// ErrStoreUnavailable, keeping the cause so callers can still tell an expired
// deadline from an outage; a reply of the wrong shape is a script bug.
func decide(cmd *redis.Cmd, limit int64) (Decision, error) {
	if err := cmd.Err(); err != nil {
		return Decision{}, fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}
	reply, err := cmd.Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("limiter: unexpected script reply: %w", err)
	}
	return decisionFromReply(limit, reply)
}
