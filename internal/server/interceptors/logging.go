package interceptors

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// healthPrefix covers every method of the standard health service. Probes call
// it every few seconds and would drown the real traffic.
const healthPrefix = "/grpc.health.v1.Health/"

// Logging writes one structured line per RPC: method, code, duration and peer.
//
// The request is never logged. Its key can be a user id or an IP, and logs
// reach more people than metrics do.
func Logging(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, healthPrefix) {
			return handler(ctx, req)
		}

		start := time.Now()
		resp, err := handler(ctx, req)
		code := status.Code(err)

		log.LogAttrs(ctx, levelFor(code), "rpc",
			slog.String("method", info.FullMethod),
			slog.String("code", code.String()),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			slog.String("peer", peerAddr(ctx)),
		)
		return resp, err
	}
}

// levelFor keeps successful traffic out of the default (info) log: metrics count
// it, logs are for exceptions. Client mistakes and a store outage are warnings;
// anything else, including Internal and unknown codes, is our problem.
func levelFor(c codes.Code) slog.Level {
	switch c {
	case codes.OK:
		return slog.LevelDebug
	case codes.InvalidArgument, codes.NotFound, codes.Canceled, codes.DeadlineExceeded, codes.Unavailable:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}
