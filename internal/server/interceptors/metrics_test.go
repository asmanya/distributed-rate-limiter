package interceptors

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/asmanya/distributed-rate-limiter/internal/metrics"
)

func newMetrics(t *testing.T) *metrics.Metrics {
	t.Helper()
	m, err := metrics.New()
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	return m
}

// histogramCount reads how many observations a histogram has for one method.
func histogramCount(t *testing.T, m *metrics.Metrics, method string) uint64 {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "grpc_server_handling_seconds" {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetName() == "method" && l.GetValue() == method {
					return metric.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

func TestMetricsCountsThreeCalls(t *testing.T) {
	m := newMetrics(t)
	intercept := Metrics(m)

	for range 3 {
		if _, err := intercept(context.Background(), nil, testInfo, failWith(codes.OK)); err != nil {
			t.Fatalf("call: %v", err)
		}
	}

	if got := testutil.ToFloat64(m.RPCHandled.WithLabelValues(testInfo.FullMethod, "OK")); got != 3 {
		t.Errorf("handled{OK} = %v, want 3", got)
	}
	if got := histogramCount(t, m, testInfo.FullMethod); got != 3 {
		t.Errorf("duration samples = %d, want 3", got)
	}
}

func TestMetricsCodeLabel(t *testing.T) {
	for _, c := range []codes.Code{codes.OK, codes.NotFound, codes.Unavailable, codes.Internal} {
		t.Run(c.String(), func(t *testing.T) {
			m := newMetrics(t)

			_, _ = Metrics(m)(context.Background(), nil, testInfo, failWith(c))

			if got := testutil.ToFloat64(m.RPCHandled.WithLabelValues(testInfo.FullMethod, c.String())); got != 1 {
				t.Errorf("handled{%s} = %v, want 1", c, got)
			}
			// Only that one series exists: the other codes were never touched.
			if got := testutil.CollectAndCount(m.RPCHandled); got != 1 {
				t.Errorf("series = %d, want 1", got)
			}
		})
	}
}

func TestMetricsSkipsHealth(t *testing.T) {
	m := newMetrics(t)
	info := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}

	resp, err := Metrics(m)(context.Background(), nil, info, failWith(codes.OK))

	if err != nil || resp != "ok" {
		t.Fatalf("got %v, %v; want ok, nil", resp, err)
	}
	if got := testutil.CollectAndCount(m.RPCHandled); got != 0 {
		t.Errorf("health check was counted: %d series", got)
	}
}

func TestMetricsPassesResultThrough(t *testing.T) {
	resp, err := Metrics(newMetrics(t))(context.Background(), nil, testInfo, failWith(codes.NotFound))
	if resp != nil || status.Code(err) != codes.NotFound {
		t.Fatalf("got %v, %v; want nil, NotFound", resp, err)
	}
}
