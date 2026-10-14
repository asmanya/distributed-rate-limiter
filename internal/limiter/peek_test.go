//go:build integration

package limiter

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type algorithm struct {
	name string
	new  func(rdb *redis.Client) Limiter
}

func algorithms() []algorithm {
	return []algorithm{
		{"token bucket", func(rdb *redis.Client) Limiter { return NewTokenBucket(rdb) }},
		{"sliding window", func(rdb *redis.Client) Limiter { return NewSlidingWindow(rdb) }},
	}
}

func newAlgo(t *testing.T, a algorithm) (Limiter, Request, *redis.Client) {
	t.Helper()
	rdb := testClient(t)
	req := Request{
		Namespace: uniqueNamespace(t),
		Policy:    "p",
		Key:       "user-1",
		Limit:     10,
		Period:    time.Minute,
	}
	t.Cleanup(func() {
		for _, k := range keysUnder(rdb, req.Namespace) {
			rdb.Del(context.Background(), k)
		}
	})
	return a.new(rdb), req, rdb
}

// keysUnder lists this test's keys with SCAN; we never touch other namespaces.
func keysUnder(rdb *redis.Client, ns string) []string {
	ctx := context.Background()
	var keys []string
	iter := rdb.Scan(ctx, 0, "rl:{"+ns+":*", 100).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	return keys
}

// snapshot maps every key of the namespace to its DUMP: a byte-for-byte copy of
// the stored value. TTL is left out on purpose, it shrinks with the real clock.
func snapshot(t *testing.T, rdb *redis.Client, ns string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, k := range keysUnder(rdb, ns) {
		v, err := rdb.Dump(context.Background(), k).Result()
		if err != nil {
			t.Fatalf("DUMP %s: %v", k, err)
		}
		out[k] = v
	}
	return out
}

func callAt(t *testing.T, fn func(context.Context, Request) (Decision, error), req Request, offset time.Duration, cost int64) Decision {
	t.Helper()
	req.Now = swAt(offset)
	req.Cost = cost
	d, err := fn(context.Background(), req)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return d
}

func TestPeekIsReadOnly(t *testing.T) {
	for _, a := range algorithms() {
		t.Run(a.name, func(t *testing.T) {
			l, req, rdb := newAlgo(t, a)

			for i := 0; i < 100; i++ {
				callAt(t, l.Peek, req, time.Duration(i)*100*time.Millisecond, 1)
			}
			if got := snapshot(t, rdb, req.Namespace); len(got) != 0 {
				t.Fatalf("peek on a fresh key created %d keys", len(got))
			}

			for i := 0; i < 3; i++ {
				callAt(t, l.Allow, req, 0, 1)
			}
			before := snapshot(t, rdb, req.Namespace)
			for i := 0; i < 100; i++ {
				callAt(t, l.Peek, req, time.Duration(i)*100*time.Millisecond, 1)
			}
			if after := snapshot(t, rdb, req.Namespace); !maps.Equal(before, after) {
				t.Fatalf("peek changed stored state")
			}
		})
	}
}

// Peek and Allow hold the same math in two scripts. This test is what keeps
// that duplication safe: from random states, Peek must equal the Allow that
// follows it. The seed is fixed so a failure can be reproduced.
func TestPeekMatchesAllow(t *testing.T) {
	for _, a := range algorithms() {
		t.Run(a.name, func(t *testing.T) {
			l, req, _ := newAlgo(t, a)
			rng := rand.New(rand.NewSource(42))
			for i := 0; i < 200; i++ {
				req.Key = fmt.Sprintf("k%d", i)
				var now time.Duration
				for s := rng.Intn(13); s > 0; s-- {
					now += time.Duration(rng.Intn(4000)) * time.Millisecond
					callAt(t, l.Allow, req, now, 1+rng.Int63n(3))
				}
				now += time.Duration(rng.Intn(4000)) * time.Millisecond
				cost := 1 + rng.Int63n(req.Limit)

				peek := callAt(t, l.Peek, req, now, cost)
				allow := callAt(t, l.Allow, req, now, cost)
				if peek != allow {
					t.Fatalf("iteration %d: peek %+v != allow %+v", i, peek, allow)
				}
			}
		})
	}
}

func TestReset(t *testing.T) {
	for _, a := range algorithms() {
		t.Run(a.name, func(t *testing.T) {
			l, req, _ := newAlgo(t, a)
			for i := 0; i < 10; i++ {
				callAt(t, l.Allow, req, 0, 1)
			}
			if d := callAt(t, l.Allow, req, 0, 1); d.Allowed {
				t.Fatalf("expected denial before reset, got %+v", d)
			}

			req.Now = swAt(0)
			if err := l.Reset(context.Background(), req); err != nil {
				t.Fatalf("Reset: %v", err)
			}
			if d := callAt(t, l.Allow, req, 0, 1); !d.Allowed || d.Remaining != 9 {
				t.Fatalf("after reset got %+v, want allowed with remaining 9", d)
			}

			// Idempotent: resetting a key that has no state is not an error.
			req.Key = "never-used"
			if err := l.Reset(context.Background(), req); err != nil {
				t.Fatalf("Reset of a missing key: %v", err)
			}
		})
	}
}
