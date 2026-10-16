// Package interceptors holds the gRPC server interceptors, written by hand.
package interceptors

import (
	"context"
	"log/slog"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Recovery turns a handler panic into an Internal error, so one bad request
// cannot take the process down.
//
// recover only catches a panic in the goroutine it runs in. A goroutine started
// inside a handler is not covered, so handlers must not spawn fire-and-forget
// goroutines.
func Recovery(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				// The stack goes to the log only; the client gets a generic
				// message, never the panic value.
				log.ErrorContext(ctx, "panic recovered",
					"method", info.FullMethod,
					"panic", r,
					"stack", string(debug.Stack()),
				)
				resp, err = nil, status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}
