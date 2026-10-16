package interceptors

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/asmanya/distributed-rate-limiter/internal/metrics"
)

// Metrics counts every RPC by method and gRPC code and times it by method.
// Health checks are skipped, as in Logging: probes would drown the real traffic.
//
// Labels are the method and the code only, both bounded sets. The request is
// never read, so its key cannot become a label.
func Metrics(m *metrics.Metrics) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, healthPrefix) {
			return handler(ctx, req)
		}

		start := time.Now()
		resp, err := handler(ctx, req)

		m.RPCHandled.WithLabelValues(info.FullMethod, status.Code(err).String()).Inc()
		m.RPCDuration.WithLabelValues(info.FullMethod).Observe(time.Since(start).Seconds())
		return resp, err
	}
}
