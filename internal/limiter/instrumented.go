package limiter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/asmanya/distributed-rate-limiter/internal/metrics"
)

// Instrumented wraps a Limiter with the Redis timeout and the Redis metrics.
//
// It holds no clock of its own: now is injected, so this package still never
// calls time.Now (locked rule 3).
type Instrumented struct {
	next    Limiter
	prefix  string // "tb" or "sw"; the metric label is prefix + "_allow" / "_peek"
	timeout time.Duration
	now     func() time.Time
	metrics *metrics.Metrics
}

var _ Limiter = (*Instrumented)(nil)

// NewInstrumented wraps next. timeout is REDIS_TIMEOUT: every call gets a child
// context with that limit (a shorter deadline from the caller still wins).
func NewInstrumented(next Limiter, prefix string, timeout time.Duration, now func() time.Time, m *metrics.Metrics) *Instrumented {
	return &Instrumented{next: next, prefix: prefix, timeout: timeout, now: now, metrics: m}
}

// Allow decides and consumes, bounded by the timeout.
func (i *Instrumented) Allow(ctx context.Context, req Request) (Decision, error) {
	return i.call(ctx, i.prefix+"_allow", func(c context.Context) (Decision, error) {
		return i.next.Allow(c, req)
	})
}

// Peek decides without consuming, bounded by the timeout.
func (i *Instrumented) Peek(ctx context.Context, req Request) (Decision, error) {
	return i.call(ctx, i.prefix+"_peek", func(c context.Context) (Decision, error) {
		return i.next.Peek(c, req)
	})
}

// Reset is bounded by the timeout like the rest. It is not measured: the metric
// labels cover the decision scripts only.
func (i *Instrumented) Reset(ctx context.Context, req Request) error {
	cctx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()

	err := i.next.Reset(cctx, req)
	if err == nil {
		return nil
	}
	_, err = i.classify(ctx, cctx, err)
	return err
}

func (i *Instrumented) call(ctx context.Context, script string, f func(context.Context) (Decision, error)) (Decision, error) {
	cctx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()

	start := i.now()
	d, err := f(cctx)
	if err == nil {
		i.observe(script, start)
		return d, nil
	}

	storeFailure, err := i.classify(ctx, cctx, err)
	if storeFailure {
		i.observe(script, start)
		i.metrics.RedisErrors.WithLabelValues(script).Inc()
	}
	return Decision{}, err
}

func (i *Instrumented) observe(script string, start time.Time) {
	i.metrics.RedisDuration.WithLabelValues(script).Observe(i.now().Sub(start).Seconds())
}

// classify tells a store failure from everything else and, for our own timeout,
// rewrites the error.
//
// Order matters: the caller's context is checked first. If the caller gave up,
// that is not the store's fault and the error is returned as it is, so it maps
// to Canceled or DeadlineExceeded. Only then does our own timeout count: it must
// become a plain ErrStoreUnavailable, without context.DeadlineExceeded in the
// chain, or the service would report the caller's deadline instead of Unavailable.
func (i *Instrumented) classify(parent, child context.Context, err error) (storeFailure bool, out error) {
	if parent.Err() != nil {
		return false, err
	}
	if !errors.Is(err, ErrStoreUnavailable) {
		return false, err // validation or a bug: it never reached Redis
	}
	if child.Err() != nil {
		return true, fmt.Errorf("%w: redis call exceeded %s", ErrStoreUnavailable, i.timeout)
	}
	return true, err
}
