package limiter

import (
	"testing"
	"time"
)

func TestTokenBucketKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"plain key", "user-1", "rl:{orders-api:search:user-1}:tb"},
		{"colon in key is allowed", "a:b", "rl:{orders-api:search:a:b}:tb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validRequest()
			req.Key = tt.key
			if got := tokenBucketKey(req); got != tt.want {
				t.Fatalf("tokenBucketKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSlidingWindowKeys(t *testing.T) {
	req := validRequest()
	req.Period = time.Minute
	req.Now = time.UnixMilli(125_000) // window 2 starts at 120_000

	cur, prev, start := slidingWindowKeys(req)

	if want := "rl:{orders-api:search:user-1}:sw:2"; cur != want {
		t.Errorf("current = %q, want %q", cur, want)
	}
	if want := "rl:{orders-api:search:user-1}:sw:1"; prev != want {
		t.Errorf("previous = %q, want %q", prev, want)
	}
	if start != 120_000 {
		t.Errorf("start = %d, want 120000", start)
	}
}

func TestWindowOfBoundary(t *testing.T) {
	tests := []struct {
		name      string
		nowMS     int64
		wantIndex int64
		wantStart int64
	}{
		{"start of window 0", 0, 0, 0},
		{"last ms of window 1", 119_999, 1, 60_000},
		{"first ms of window 2", 120_000, 2, 120_000},
		{"mid window 2", 125_000, 2, 120_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validRequest()
			req.Period = time.Minute
			req.Now = time.UnixMilli(tt.nowMS)
			index, start := windowOf(req)
			if index != tt.wantIndex || start != tt.wantStart {
				t.Fatalf("windowOf() = (%d, %d), want (%d, %d)", index, start, tt.wantIndex, tt.wantStart)
			}
		})
	}
}
