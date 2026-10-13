package limiter

import (
	_ "embed"

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
