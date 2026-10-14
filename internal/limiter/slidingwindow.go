package limiter

import (
	"context"
	"fmt"
	"time"
)

// SlidingWindow is the sliding window counter algorithm backed by Lua scripts.
// It is an approximation: the previous window counts in proportion to how much
// of it still overlaps the sliding window.
type SlidingWindow struct {
	rdb Client
}

var _ Limiter = (*SlidingWindow)(nil)

// NewSlidingWindow returns a SlidingWindow that runs its scripts on rdb.
func NewSlidingWindow(rdb Client) *SlidingWindow {
	return &SlidingWindow{rdb: rdb}
}

// Allow decides whether req may proceed and, if so, counts req.Cost against
// the current window.
func (s *SlidingWindow) Allow(ctx context.Context, req Request) (Decision, error) {
	if err := req.Validate(); err != nil {
		return Decision{}, err
	}
	keys, args := slidingWindowCall(req)
	return decide(slidingWindowScript.Run(ctx, s.rdb, keys, args...), req.Limit)
}

// Peek returns the decision Allow would make right now without changing state.
func (s *SlidingWindow) Peek(ctx context.Context, req Request) (Decision, error) {
	if err := req.Validate(); err != nil {
		return Decision{}, err
	}
	keys, args := slidingWindowCall(req)
	return decide(slidingWindowPeekScript.RunRO(ctx, s.rdb, keys, args...), req.Limit)
}

// Reset deletes the current and previous window counters. Older windows no
// longer count and expire on their own. Missing keys are not an error.
func (s *SlidingWindow) Reset(ctx context.Context, req Request) error {
	if err := req.validateTarget(); err != nil {
		return err
	}
	if req.Period < time.Millisecond { // needed to locate the windows
		return ErrInvalidPolicy
	}
	current, previous, _ := slidingWindowKeys(req)
	if err := s.rdb.Del(ctx, current, previous).Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}
	return nil
}

// slidingWindowCall builds the KEYS and ARGV shared by Allow and Peek. Window
// index and start come from one place (windowOf), so the keys and the script
// argument always describe the same window.
func slidingWindowCall(req Request) ([]string, []any) {
	current, previous, startMS := slidingWindowKeys(req)
	return []string{current, previous},
		[]any{req.Limit, req.Period.Milliseconds(), req.Cost, req.nowMillis(), startMS}
}
