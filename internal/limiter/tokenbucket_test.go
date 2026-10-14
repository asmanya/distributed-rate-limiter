//go:build integration

package limiter

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Capacity 10 over 10s is 1 token per second, so every expected value below
// can be checked by hand.
const baseMS = 1_700_000_000_000

// at returns the injected "now": baseMS plus an offset. No test sleeps.
func at(offset time.Duration) time.Time {
	return time.UnixMilli(baseMS).Add(offset)
}

func newTestBucket(t *testing.T) (*TokenBucket, Request, *redis.Client) {
	t.Helper()
	rdb := testClient(t)
	req := Request{
		Namespace: uniqueNamespace(t),
		Policy:    "search",
		Key:       "user-1",
		Limit:     10,
		Period:    10 * time.Second,
	}
	t.Cleanup(func() { rdb.Del(context.Background(), tokenBucketKey(req)) })
	return NewTokenBucket(rdb), req, rdb
}

func allowAt(t *testing.T, b *TokenBucket, req Request, offset time.Duration, cost int64) Decision {
	t.Helper()
	req.Now = at(offset)
	req.Cost = cost
	d, err := b.Allow(context.Background(), req)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	return d
}

// drain consumes n single tokens at t=0 and requires each to be allowed.
func drain(t *testing.T, b *TokenBucket, req Request, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if d := allowAt(t, b, req, 0, 1); !d.Allowed {
			t.Fatalf("drain call %d denied: %+v", i+1, d)
		}
	}
}

func TestTokenBucketAllow(t *testing.T) {
	t.Run("fresh key", func(t *testing.T) {
		b, req, _ := newTestBucket(t)
		got := allowAt(t, b, req, 0, 1)
		want := Decision{Allowed: true, Limit: 10, Remaining: 9, ResetAfter: time.Second}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("ten allowed then eleventh denied", func(t *testing.T) {
		b, req, _ := newTestBucket(t)
		for i := 0; i < 10; i++ {
			d := allowAt(t, b, req, 0, 1)
			if !d.Allowed || d.Remaining != int64(9-i) {
				t.Fatalf("call %d: got %+v, want allowed with remaining %d", i+1, d, 9-i)
			}
		}
		d := allowAt(t, b, req, 0, 1)
		if d.Allowed || d.Remaining != 0 || d.RetryAfter != time.Second {
			t.Fatalf("eleventh: got %+v, want denied, remaining 0, retry_after 1s", d)
		}
	})

	t.Run("denied half a second after exhaust", func(t *testing.T) {
		b, req, _ := newTestBucket(t)
		drain(t, b, req, 10)
		d := allowAt(t, b, req, 500*time.Millisecond, 1)
		if d.Allowed || d.RetryAfter != 500*time.Millisecond {
			t.Fatalf("got %+v, want denied with retry_after 500ms", d)
		}
	})

	t.Run("allowed one second after exhaust", func(t *testing.T) {
		b, req, _ := newTestBucket(t)
		drain(t, b, req, 10)
		d := allowAt(t, b, req, time.Second, 1)
		if !d.Allowed || d.Remaining != 0 {
			t.Fatalf("got %+v, want allowed with remaining 0", d)
		}
	})

	t.Run("denied request leaves tokens untouched", func(t *testing.T) {
		b, req, _ := newTestBucket(t)
		drain(t, b, req, 7) // 3 tokens left
		d := allowAt(t, b, req, 0, 5)
		if d.Allowed || d.RetryAfter != 2*time.Second {
			t.Fatalf("cost 5: got %+v, want denied with retry_after 2s", d)
		}
		// If the denial had written anything, these 3 tokens would be gone.
		d = allowAt(t, b, req, 0, 3)
		if !d.Allowed || d.Remaining != 0 {
			t.Fatalf("cost 3: got %+v, want allowed with remaining 0", d)
		}
	})

	t.Run("now before stored timestamp", func(t *testing.T) {
		b, req, _ := newTestBucket(t)
		allowAt(t, b, req, 0, 1) // stores ts = t0, 9 tokens
		d := allowAt(t, b, req, -5*time.Second, 1)
		// Negative elapsed is clamped to zero: no refill, no crash.
		if !d.Allowed || d.Remaining != 8 {
			t.Fatalf("got %+v, want allowed with remaining 8", d)
		}
	})

	t.Run("ttl is bounded by full refill time", func(t *testing.T) {
		b, req, rdb := newTestBucket(t)
		drain(t, b, req, 10) // empty bucket: full refill takes 10s
		ttl, err := rdb.PTTL(context.Background(), tokenBucketKey(req)).Result()
		if err != nil {
			t.Fatalf("PTTL: %v", err)
		}
		if ttl <= 0 || ttl > 10*time.Second {
			t.Fatalf("ttl = %v, want in (0, 10s]", ttl)
		}
	})

	t.Run("ten seconds after exhaust", func(t *testing.T) {
		b, req, _ := newTestBucket(t)
		drain(t, b, req, 10)
		// Whether or not the key expired, the bucket is full by now.
		d := allowAt(t, b, req, 10*time.Second, 1)
		if !d.Allowed || d.Remaining != 9 {
			t.Fatalf("got %+v, want allowed with remaining 9", d)
		}
	})
}
