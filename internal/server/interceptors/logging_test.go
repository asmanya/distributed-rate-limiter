package interceptors

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func debugLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func failWith(c codes.Code) grpc.UnaryHandler {
	return func(context.Context, any) (any, error) {
		if c == codes.OK {
			return "ok", nil
		}
		return nil, status.Error(c, "x")
	}
}

func TestLoggingLevels(t *testing.T) {
	tests := []struct {
		code      codes.Code
		wantLevel string
	}{
		{codes.OK, "level=DEBUG"},
		{codes.InvalidArgument, "level=WARN"},
		{codes.NotFound, "level=WARN"},
		{codes.Canceled, "level=WARN"},
		{codes.DeadlineExceeded, "level=WARN"},
		{codes.Unavailable, "level=WARN"},
		{codes.Internal, "level=ERROR"},
		{codes.DataLoss, "level=ERROR"}, // unknown to us: our problem
	}
	for _, tt := range tests {
		t.Run(tt.code.String(), func(t *testing.T) {
			var buf bytes.Buffer
			_, _ = Logging(debugLogger(&buf))(context.Background(), nil, testInfo, failWith(tt.code))

			out := buf.String()
			for _, want := range []string{tt.wantLevel, "code=" + tt.code.String(), "method=" + testInfo.FullMethod, "duration_ms="} {
				if !strings.Contains(out, want) {
					t.Errorf("log missing %q: %s", want, out)
				}
			}
		})
	}
}

func TestLoggingSkipsHealth(t *testing.T) {
	var buf bytes.Buffer
	info := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}

	resp, err := Logging(debugLogger(&buf))(context.Background(), nil, info, failWith(codes.OK))

	if err != nil || resp != "ok" {
		t.Fatalf("got %v, %v; want ok, nil", resp, err)
	}
	if buf.Len() != 0 {
		t.Errorf("health check was logged: %s", buf.String())
	}
}

func TestLoggingOKIsDroppedAtInfo(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	_, _ = Logging(log)(context.Background(), nil, testInfo, failWith(codes.OK))

	if buf.Len() != 0 {
		t.Errorf("successful RPC logged at info: %s", buf.String())
	}
}

func TestLoggingPassesResultThrough(t *testing.T) {
	var buf bytes.Buffer
	resp, err := Logging(debugLogger(&buf))(context.Background(), nil, testInfo, failWith(codes.NotFound))
	if resp != nil || status.Code(err) != codes.NotFound {
		t.Fatalf("got %v, %v; want nil, NotFound", resp, err)
	}
}

func TestLoggingPeer(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{
			"with peer",
			peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(10, 1, 2, 3), Port: 5555}}),
			"peer=10.1.2.3:5555",
		},
		{"no peer", context.Background(), "peer=unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			_, _ = Logging(debugLogger(&buf))(tt.ctx, nil, testInfo, failWith(codes.OK))
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("log missing %q: %s", tt.want, buf.String())
			}
		})
	}
}

// The key can be a user id or an IP, so it must not reach the log, even at debug.
func TestLoggingNeverLogsTheRequest(t *testing.T) {
	type req struct{ Key string }
	var buf bytes.Buffer

	_, _ = Logging(debugLogger(&buf))(context.Background(), &req{Key: "user-secret-123"}, testInfo, failWith(codes.OK))

	if buf.Len() == 0 {
		t.Fatal("nothing logged, the check proves nothing")
	}
	if strings.Contains(buf.String(), "user-secret-123") {
		t.Errorf("log leaks the key: %s", buf.String())
	}
}
