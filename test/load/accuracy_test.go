//go:build integration

package load_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/asmanya/distributed-rate-limiter/internal/limiter"
)

const (
	limit      = 500
	goroutines = 200
	callsEach  = 10 // 200 * 10 = 2000 attempts against a limit of 500
)

// fixedNow never moves. With a real clock, tokens would refill during the test
// and the count could pass 500 even though the code is right.
var fixedNow = time.UnixMilli(1_700_000_040_000)

func newClient(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; run `make up` and export it")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_URL: %v", err)
	}
	c := redis.NewClient(opts)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	return c
}

func uniqueNamespace(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return "accuracy-" + hex.EncodeToString(b)
}

// hammer runs goroutines concurrently, spreading them across limiters, and
// counts the outcomes. It reports errors with t.Errorf: t.Fatal must not be
// called from a goroutine other than the test's own.
func hammer(t *testing.T, limiters []limiter.Limiter, req limiter.Request) (allowed, denied int64) {
	t.Helper()
	var wg sync.WaitGroup
	var nAllowed, nDenied atomic.Int64
	for g := 0; g < goroutines; g++ {
		l := limiters[g%len(limiters)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < callsEach; i++ {
				d, err := l.Allow(context.Background(), req)
				if err != nil {
					t.Errorf("Allow: %v", err)
					return
				}
				if d.Allowed {
					nAllowed.Add(1)
				} else {
					nDenied.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	return nAllowed.Load(), nDenied.Load()
}

func TestAccuracy(t *testing.T) {
	algorithms := []struct {
		name string
		new  func(c limiter.Client) limiter.Limiter
	}{
		{"token bucket", func(c limiter.Client) limiter.Limiter { return limiter.NewTokenBucket(c) }},
		{"sliding window", func(c limiter.Client) limiter.Limiter { return limiter.NewSlidingWindow(c) }},
	}

	for _, a := range algorithms {
		for _, clients := range []int{1, 2} {
			name := a.name + "/one client"
			if clients == 2 {
				// Two clients stand in for two service instances sharing one Redis.
				name = a.name + "/two clients"
			}
			t.Run(name, func(t *testing.T) {
				limiters := make([]limiter.Limiter, clients)
				for i := range limiters {
					limiters[i] = a.new(newClient(t))
				}
				req := limiter.Request{
					Namespace: uniqueNamespace(t),
					Policy:    "accuracy",
					Key:       "hot",
					Limit:     limit,
					// Long on purpose. The clock is fixed, but the key's TTL (time to a full
					// refill) runs in real time. With a short period the TTL is a few
					// hundred ms, and on a Redis with a slow round trip the key expires
					// mid-test and the bucket starts full again: more than 500 allowed.
					Period: time.Hour,
					Cost:   1,
					Now:    fixedNow,
				}
				t.Cleanup(func() { _ = limiters[0].Reset(context.Background(), req) })

				allowed, denied := hammer(t, limiters, req)

				if allowed != limit {
					t.Errorf("allowed = %d, want exactly %d", allowed, limit)
				}
				if want := int64(goroutines*callsEach - limit); denied != want {
					t.Errorf("denied = %d, want %d", denied, want)
				}
			})
		}
	}
}
