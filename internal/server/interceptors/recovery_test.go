package interceptors

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var testInfo = &grpc.UnaryServerInfo{FullMethod: "/ratelimiter.v1.RateLimitService/Allow"}

func TestRecovery(t *testing.T) {
	notFound := status.Error(codes.NotFound, "policy not found")

	tests := []struct {
		name     string
		handler  grpc.UnaryHandler
		wantResp any
		wantCode codes.Code
		wantMsg  string
	}{
		{
			"normal",
			func(context.Context, any) (any, error) { return "ok", nil },
			"ok", codes.OK, "",
		},
		{
			"handler error passes through",
			func(context.Context, any) (any, error) { return nil, notFound },
			nil, codes.NotFound, "policy not found",
		},
		{
			"panic with string",
			func(context.Context, any) (any, error) { panic("boom") },
			nil, codes.Internal, "internal error",
		},
		{
			"panic with error",
			func(context.Context, any) (any, error) { panic(errors.New("x")) },
			nil, codes.Internal, "internal error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, nil))

			resp, err := Recovery(log)(context.Background(), nil, testInfo, tt.handler)

			if resp != tt.wantResp {
				t.Errorf("resp = %v, want %v", resp, tt.wantResp)
			}
			if got := status.Code(err); got != tt.wantCode {
				t.Fatalf("code = %v (%v), want %v", got, err, tt.wantCode)
			}
			if tt.wantMsg != "" && status.Convert(err).Message() != tt.wantMsg {
				t.Errorf("message = %q, want %q", status.Convert(err).Message(), tt.wantMsg)
			}
			if tt.wantCode != codes.Internal && buf.Len() != 0 {
				t.Errorf("unexpected log output: %s", buf.String())
			}
		})
	}
}

func TestRecoveryProcessSurvives(t *testing.T) {
	intercept := Recovery(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	_, err := intercept(context.Background(), nil, testInfo,
		func(context.Context, any) (any, error) { panic("boom") })
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("panic call: code = %v, want Internal", got)
	}

	// The same interceptor still serves the next request.
	resp, err := intercept(context.Background(), nil, testInfo,
		func(context.Context, any) (any, error) { return "ok", nil })
	if err != nil || resp != "ok" {
		t.Fatalf("next call = %v, %v; want ok, nil", resp, err)
	}
}

func TestRecoveryLogsButDoesNotLeak(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	_, err := Recovery(log)(context.Background(), nil, testInfo,
		func(context.Context, any) (any, error) { panic("boom") })

	out := buf.String()
	for _, want := range []string{"level=ERROR", "panic recovered", "boom", "stack=", testInfo.FullMethod} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q: %s", want, out)
		}
	}
	if msg := status.Convert(err).Message(); strings.Contains(msg, "boom") {
		t.Errorf("client message leaks the panic value: %q", msg)
	}
}
