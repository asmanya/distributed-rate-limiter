package limiter

import (
	"context"
	"strings"
	"time"
)

const (
	maxKeyLen  = 256
	maxNameLen = 64
)

// Request is one limiting decision to make. Limit and Period come with the
// request so the limiter stays stateless and never reads config; a changed
// policy applies on the very next call.
type Request struct {
	Namespace string
	Policy    string
	Key       string
	Limit     int64
	Period    time.Duration
	Cost      int64
	// Now is supplied by the caller; the limiter never reads the clock.
	Now time.Time
}

// Decision is the outcome of Allow or Peek.
type Decision struct {
	Allowed    bool
	Limit      int64
	Remaining  int64
	RetryAfter time.Duration
	ResetAfter time.Duration
}

// Limiter makes rate limiting decisions. Both algorithms implement it.
type Limiter interface {
	// Allow decides and, if allowed, consumes req.Cost units.
	Allow(ctx context.Context, req Request) (Decision, error)
	// Peek returns what Allow would decide right now without changing state.
	Peek(ctx context.Context, req Request) (Decision, error)
	// Reset deletes the stored state for the key.
	Reset(ctx context.Context, req Request) error
}

// Validate checks the request in a fixed order: names, key, policy
// parameters, cost.
func (r Request) Validate() error {
	if err := r.validateTarget(); err != nil {
		return err
	}
	if r.Limit <= 0 || r.Period < time.Millisecond {
		return ErrInvalidPolicy
	}
	if r.Cost <= 0 {
		return ErrInvalidCost
	}
	if r.Cost > r.Limit {
		return ErrCostExceedsLimit
	}
	return nil
}

// validateTarget checks only what identifies the key. Reset needs no cost or
// limit, so it validates just this.
func (r Request) validateTarget() error {
	if !validName(r.Namespace) || !validName(r.Policy) {
		return ErrInvalidName
	}
	if r.Key == "" || len(r.Key) > maxKeyLen || strings.ContainsAny(r.Key, "{}") {
		return ErrInvalidKey
	}
	return nil
}

// validName reports whether s is usable as a namespace or policy. ':' would
// let two different limiters build the same Redis key.
func validName(s string) bool {
	return s != "" && len(s) <= maxNameLen && !strings.ContainsAny(s, ":{}")
}

// nowMillis is the only place Now becomes the milliseconds the scripts expect.
// Converting anywhere else risks a seconds/milliseconds mixup.
func (r Request) nowMillis() int64 {
	return r.Now.UnixMilli()
}
