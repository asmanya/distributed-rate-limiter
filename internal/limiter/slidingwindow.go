package limiter

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// SlidingWindow is the sliding window counter algorithm backed by one Lua
// script. It is an approximation: the previous window counts in proportion to
// how much of it still overlaps the sliding window.
type SlidingWindow struct {
	rdb redis.Scripter
}

// NewSlidingWindow returns a SlidingWindow that runs its script on rdb.
func NewSlidingWindow(rdb redis.Scripter) *SlidingWindow {
	return &SlidingWindow{rdb: rdb}
}

// Allow decides whether req may proceed and, if so, counts req.Cost against
// the current window.
func (s *SlidingWindow) Allow(ctx context.Context, req Request) (Decision, error) {
	if err := req.Validate(); err != nil {
		return Decision{}, err
	}

	// Window index and start come from one place (windowOf), so the keys and
	// the script argument always describe the same window.
	current, previous, startMS := slidingWindowKeys(req)
	cmd := slidingWindowScript.Run(ctx, s.rdb,
		[]string{current, previous},
		req.Limit, req.Period.Milliseconds(), req.Cost, req.nowMillis(), startMS,
	)
	if err := cmd.Err(); err != nil {
		return Decision{}, fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}

	reply, err := cmd.Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("limiter: unexpected script reply: %w", err)
	}
	return decisionFromReply(req.Limit, reply)
}
