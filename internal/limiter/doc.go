// Package limiter decides whether a key may consume cost units right now.
// It implements two algorithms, token bucket and sliding window counter,
// each as a single atomic Lua script run in Redis.
//
// The package knows nothing about gRPC, config files or HTTP; the current
// time is supplied by the caller.
package limiter
