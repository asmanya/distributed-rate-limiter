package limiter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/asmanya/distributed-rate-limiter/internal/metrics"
)

// stubLimiter returns a fixed result, or blocks until its context ends when
// block is set, and records the context it was given.
type stubLimiter struct {
	err   error
	block bool

	deadline    time.Time
	hasDeadline bool
}

func (s *stubLimiter) run(ctx context.Context) error {
	s.deadline, s.hasDeadline = ctx.Deadline()
	if s.block {
		<-ctx.Done()
		return fmt.Errorf("%w: %w", ErrStoreUnavailable, ctx.Err())
	}
	return s.err
}

func (s *stubLimiter) Allow(ctx context.Context, _ Request) (Decision, error) {
	return Decision{Allowed: true}, s.run(ctx)
}

func (s *stubLimiter) Peek(ctx context.Context, _ Request) (Decision, error) {
	return Decision{Allowed: true}, s.run(ctx)
}

func (s *stubLimiter) Reset(ctx context.Context, _ Request) error { return s.run(ctx) }

// steppingClock moves 5ms on every read, so durations are exact and nothing sleeps.
func steppingClock() func() time.Time {
	t := time.UnixMilli(1_700_000_000_000)
	return func() time.Time {
		t = t.Add(5 * time.Millisecond)
		return t
	}
}

func newInstrumented(t *testing.T, s *stubLimiter, prefix string, timeout time.Duration) (*Instrumented, *metrics.Metrics) {
	t.Helper()
	m, err := metrics.New()
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	return NewInstrumented(s, prefix, timeout, steppingClock(), m), m
}

// durationSamples counts histogram observations for one script label.
func durationSamples(t *testing.T, m *metrics.Metrics, script string) uint64 {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "ratelimiter_redis_duration_seconds" {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetName() == "script" && l.GetValue() == script {
					return metric.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

func TestInstrumentedSuccess(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		call   func(*Instrumented) error
		script string
	}{
		{"tb allow", "tb", func(i *Instrumented) error { _, err := i.Allow(context.Background(), validRequest()); return err }, "tb_allow"},
		{"tb peek", "tb", func(i *Instrumented) error { _, err := i.Peek(context.Background(), validRequest()); return err }, "tb_peek"},
		{"sw allow", "sw", func(i *Instrumented) error { _, err := i.Allow(context.Background(), validRequest()); return err }, "sw_allow"},
		{"sw peek", "sw", func(i *Instrumented) error { _, err := i.Peek(context.Background(), validRequest()); return err }, "sw_peek"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i, m := newInstrumented(t, &stubLimiter{}, tt.prefix, 50*time.Millisecond)

			if err := tt.call(i); err != nil {
				t.Fatalf("call: %v", err)
			}

			if got := durationSamples(t, m, tt.script); got != 1 {
				t.Errorf("duration samples{%s} = %d, want 1", tt.script, got)
			}
			if got := testutil.CollectAndCount(m.RedisErrors); got != 0 {
				t.Errorf("error series = %d, want 0", got)
			}
		})
	}
}

func TestInstrumentedStoreError(t *testing.T) {
	refused := fmt.Errorf("%w: %w", ErrStoreUnavailable, errors.New("connection refused"))
	i, m := newInstrumented(t, &stubLimiter{err: refused}, "tb", 50*time.Millisecond)

	_, err := i.Allow(context.Background(), validRequest())

	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("err = %v, want ErrStoreUnavailable", err)
	}
	if got := testutil.ToFloat64(m.RedisErrors.WithLabelValues("tb_allow")); got != 1 {
		t.Errorf("errors{tb_allow} = %v, want 1", got)
	}
	if got := durationSamples(t, m, "tb_allow"); got != 1 {
		t.Errorf("duration samples = %d, want 1 (a failed call still took time)", got)
	}
}

// The plan's "two codes" distinction: our own timeout is the store's fault.
func TestInstrumentedInternalTimeoutIsUnavailable(t *testing.T) {
	i, m := newInstrumented(t, &stubLimiter{block: true}, "tb", time.Millisecond)

	_, err := i.Allow(context.Background(), validRequest())

	if !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("err = %v, want ErrStoreUnavailable", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v: DeadlineExceeded in the chain would map to the caller's deadline", err)
	}
	if got := testutil.ToFloat64(m.RedisErrors.WithLabelValues("tb_allow")); got != 1 {
		t.Errorf("errors{tb_allow} = %v, want 1", got)
	}
}

// The caller giving up is not the store's fault.
func TestInstrumentedCallerCancelIsNotAStoreError(t *testing.T) {
	i, m := newInstrumented(t, &stubLimiter{block: true}, "tb", time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := i.Allow(ctx, validRequest())

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled in the chain", err)
	}
	if got := testutil.CollectAndCount(m.RedisErrors); got != 0 {
		t.Errorf("error series = %d, want 0", got)
	}
}

func TestInstrumentedCallerDeadlineIsNotAStoreError(t *testing.T) {
	i, m := newInstrumented(t, &stubLimiter{block: true}, "tb", time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	_, err := i.Allow(ctx, validRequest())

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded in the chain", err)
	}
	if got := testutil.CollectAndCount(m.RedisErrors); got != 0 {
		t.Errorf("error series = %d, want 0", got)
	}
}

func TestInstrumentedShorterCallerDeadlineWins(t *testing.T) {
	s := &stubLimiter{}
	i, _ := newInstrumented(t, s, "tb", time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	want, _ := ctx.Deadline()

	if _, err := i.Allow(ctx, validRequest()); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	if !s.hasDeadline || !s.deadline.Equal(want) {
		t.Errorf("limiter saw deadline %v (set %v), want the caller's %v", s.deadline, s.hasDeadline, want)
	}
}

func TestInstrumentedAddsDeadlineWhenCallerHasNone(t *testing.T) {
	s := &stubLimiter{}
	i, _ := newInstrumented(t, s, "tb", 50*time.Millisecond)

	if _, err := i.Allow(context.Background(), validRequest()); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !s.hasDeadline {
		t.Error("limiter saw a context without a deadline")
	}
}

func TestInstrumentedNonStoreErrorsPassThrough(t *testing.T) {
	i, m := newInstrumented(t, &stubLimiter{err: ErrInvalidKey}, "tb", 50*time.Millisecond)

	_, err := i.Allow(context.Background(), validRequest())

	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("err = %v, want ErrInvalidKey", err)
	}
	if got := durationSamples(t, m, "tb_allow"); got != 0 {
		t.Errorf("duration samples = %d, want 0: it never reached Redis", got)
	}
	if got := testutil.CollectAndCount(m.RedisErrors); got != 0 {
		t.Errorf("error series = %d, want 0", got)
	}
}

func TestInstrumentedReset(t *testing.T) {
	t.Run("bounded and not measured", func(t *testing.T) {
		s := &stubLimiter{}
		i, m := newInstrumented(t, s, "tb", 50*time.Millisecond)

		if err := i.Reset(context.Background(), validRequest()); err != nil {
			t.Fatalf("Reset: %v", err)
		}
		if !s.hasDeadline {
			t.Error("Reset ran without a deadline")
		}
		if got := testutil.CollectAndCount(m.RedisDuration); got != 0 {
			t.Errorf("duration series = %d, want 0", got)
		}
	})

	t.Run("own timeout is Unavailable", func(t *testing.T) {
		i, _ := newInstrumented(t, &stubLimiter{block: true}, "tb", time.Millisecond)

		err := i.Reset(context.Background(), validRequest())

		if !errors.Is(err, ErrStoreUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want ErrStoreUnavailable without DeadlineExceeded", err)
		}
	})
}
