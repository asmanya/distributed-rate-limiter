//go:build integration

package limiter

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// swBase is the first millisecond of a 60s window (1_700_000_040_000 / 60000 is
// a whole number), so an offset reads as "seconds into the window" and never
// crosses a boundary by accident.
const swBase = 1_700_000_040_000

func swAt(offset time.Duration) time.Time {
	return time.UnixMilli(swBase).Add(offset)
}

func newTestWindow(t *testing.T) (*SlidingWindow, Request, *redis.Client) {
	t.Helper()
	rdb := testClient(t)
	req := Request{
		Namespace: uniqueNamespace(t),
		Policy:    "login",
		Key:       "user-1",
		Limit:     10,
		Period:    time.Minute,
	}
	t.Cleanup(func() {
		// Delete the keys of every window this test can have touched.
		for _, off := range []time.Duration{-time.Minute, 0, time.Minute} {
			r := req
			r.Now = swAt(off)
			cur, prev, _ := slidingWindowKeys(r)
			rdb.Del(context.Background(), cur, prev)
		}
	})
	return NewSlidingWindow(rdb), req, rdb
}

func swAllow(t *testing.T, w *SlidingWindow, req Request, offset time.Duration, cost int64) Decision {
	t.Helper()
	req.Now = swAt(offset)
	req.Cost = cost
	d, err := w.Allow(context.Background(), req)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	return d
}

// countAllowed makes n single-unit calls at the same instant and returns how
// many were allowed.
func countAllowed(t *testing.T, w *SlidingWindow, req Request, offset time.Duration, n int) int {
	t.Helper()
	allowed := 0
	for i := 0; i < n; i++ {
		if swAllow(t, w, req, offset, 1).Allowed {
			allowed++
		}
	}
	return allowed
}

// seed writes the current and previous window counters directly, to reach a
// state (like "previous window full") without 10 calls and a real window roll.
func seed(t *testing.T, rdb *redis.Client, req Request, offset time.Duration, curr, prev int64) {
	t.Helper()
	req.Now = swAt(offset)
	curKey, prevKey, _ := slidingWindowKeys(req)
	for key, n := range map[string]int64{curKey: curr, prevKey: prev} {
		if n == 0 {
			continue
		}
		if err := rdb.Set(context.Background(), key, n, 2*time.Minute).Err(); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
}

func TestSlidingWindowAllow(t *testing.T) {
	t.Run("ten allowed then eleventh denied", func(t *testing.T) {
		w, req, _ := newTestWindow(t)
		if got := countAllowed(t, w, req, 0, 10); got != 10 {
			t.Fatalf("allowed %d of first 10, want 10", got)
		}
		if d := swAllow(t, w, req, 0, 1); d.Allowed {
			t.Fatalf("eleventh: got %+v, want denied", d)
		}
	})

	t.Run("previous window full, thirty seconds in", func(t *testing.T) {
		w, req, rdb := newTestWindow(t)
		seed(t, rdb, req, 30*time.Second, 0, 10) // previous contributes 10 * 30/60 = 5
		if got := countAllowed(t, w, req, 30*time.Second, 10); got != 5 {
			t.Fatalf("allowed %d, want 5", got)
		}
	})

	t.Run("previous window full, fifty-four seconds in", func(t *testing.T) {
		w, req, rdb := newTestWindow(t)
		seed(t, rdb, req, 54*time.Second, 0, 10) // previous contributes 10 * 6/60 = 1
		if got := countAllowed(t, w, req, 54*time.Second, 10); got != 9 {
			t.Fatalf("allowed %d, want 9", got)
		}
	})

	t.Run("boundary burst is not doubled", func(t *testing.T) {
		w, req, _ := newTestWindow(t)
		// A fixed window would allow 10 here and 10 more right after the
		// boundary. The previous window still weighs almost 10, so none pass.
		if got := countAllowed(t, w, req, 59_900*time.Millisecond, 10); got != 10 {
			t.Fatalf("before boundary allowed %d, want 10", got)
		}
		if got := countAllowed(t, w, req, 60_100*time.Millisecond, 10); got != 0 {
			t.Fatalf("after boundary allowed %d, want 0", got)
		}
	})

	t.Run("case A: room in current window, wait for previous to fade", func(t *testing.T) {
		w, req, rdb := newTestWindow(t)
		seed(t, rdb, req, 30*time.Second, 5, 10)
		d := swAllow(t, w, req, 30*time.Second, 1)
		// (60 - 30) - 60 * (10 - 1 - 5) / 10 = 30 - 24 = 6s
		if d.Allowed || d.RetryAfter != 6*time.Second {
			t.Fatalf("got %+v, want denied with retry_after 6s", d)
		}
	})

	t.Run("case B: current window full, wait for the next", func(t *testing.T) {
		w, req, rdb := newTestWindow(t)
		seed(t, rdb, req, 20*time.Second, 10, 0)
		d := swAllow(t, w, req, 20*time.Second, 1)
		// (60 - 20) + (60 - 60 * 9 / 10) = 40 + 6 = 46s
		if d.Allowed || d.RetryAfter != 46*time.Second {
			t.Fatalf("got %+v, want denied with retry_after 46s", d)
		}
	})

	t.Run("ttl is about two periods", func(t *testing.T) {
		w, req, rdb := newTestWindow(t)
		swAllow(t, w, req, 0, 1)
		at0 := req
		at0.Now = swAt(0)
		cur, _, _ := slidingWindowKeys(at0)
		ttl, err := rdb.PTTL(context.Background(), cur).Result()
		if err != nil {
			t.Fatalf("PTTL: %v", err)
		}
		if ttl <= time.Minute || ttl > 2*time.Minute {
			t.Fatalf("ttl = %v, want in (1m, 2m]", ttl)
		}
	})
}
