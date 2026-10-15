//go:build integration

package limiter

import (
	"context"
	"testing"
	"time"
)

// The limit is huge so every call is an allowed one: the benchmark measures
// the write path (read, decide, write), not a denial.
func benchRequest(b *testing.B) Request {
	return Request{
		Namespace: uniqueNamespace(b),
		Policy:    "bench",
		Key:       "k",
		Limit:     1_000_000_000,
		Period:    time.Hour,
		Cost:      1,
		Now:       swAt(0),
	}
}

func benchmarkAllow(b *testing.B, newLimiter func(Client) Limiter) {
	ctx := context.Background()
	l := newLimiter(testClient(b))
	req := benchRequest(b)
	b.Cleanup(func() { _ = l.Reset(ctx, req) })

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.Allow(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// Every goroutine hits the same key, so this is also the hot-key case: the
// scripts for one key run one after another inside Redis.
func benchmarkAllowParallel(b *testing.B, newLimiter func(Client) Limiter) {
	ctx := context.Background()
	l := newLimiter(testClient(b))
	req := benchRequest(b)
	b.Cleanup(func() { _ = l.Reset(ctx, req) })

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := l.Allow(ctx, req); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func newTokenBucket(c Client) Limiter   { return NewTokenBucket(c) }
func newSlidingWindow(c Client) Limiter { return NewSlidingWindow(c) }

func BenchmarkTokenBucketAllow(b *testing.B)           { benchmarkAllow(b, newTokenBucket) }
func BenchmarkTokenBucketAllowParallel(b *testing.B)   { benchmarkAllowParallel(b, newTokenBucket) }
func BenchmarkSlidingWindowAllow(b *testing.B)         { benchmarkAllow(b, newSlidingWindow) }
func BenchmarkSlidingWindowAllowParallel(b *testing.B) { benchmarkAllowParallel(b, newSlidingWindow) }
