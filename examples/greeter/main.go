// Command greeter is a demo service protected by the rate limiter SDK. It uses
// only the public SDK (pkg/client), the way a real service would.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"

	greeterv1 "github.com/asmanya/distributed-rate-limiter/gen/examples/greeter/v1"
	"github.com/asmanya/distributed-rate-limiter/pkg/client"
)

const (
	// policy is defined in deploy/policies.yaml: 5 requests per 10 seconds.
	policy    = "greeter"
	namespace = "greeter"
	// shutdownTimeout is how long in-flight calls get to finish after SIGTERM.
	shutdownTimeout = 5 * time.Second
)

// greeter is the protected service: it only greets.
type greeter struct {
	greeterv1.UnimplementedGreeterServiceServer
}

func (greeter) SayHello(_ context.Context, req *greeterv1.SayHelloRequest) (*greeterv1.SayHelloResponse, error) {
	return &greeterv1.SayHelloResponse{Message: "Hello, " + req.GetName() + "!"}, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "greeter:", err)
		os.Exit(1)
	}
}

func run() error {
	// Config only from the environment. "dns:///" makes gRPC resolve every
	// address behind the name, so all limiter replicas get traffic.
	limiterAddr := env("LIMITER_ADDR", "dns:///localhost:8080")
	port := env("GREETER_PORT", "8081")

	limiter, err := client.New(limiterAddr,
		client.WithNamespace(namespace),
		// If the limiter is down, keep greeting rather than go down with it.
		client.WithFailMode(client.FailOpen),
		// Plaintext: for this demo only. A real service uses TLS credentials.
		client.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("limiter client: %w", err)
	}
	defer func() { _ = limiter.Close() }()

	// Limit SayHello by the x-api-key metadata entry; a caller that sends none
	// is limited by its IP address instead.
	limits := map[string]string{greeterv1.GreeterService_SayHello_FullMethodName: policy}
	srv := grpc.NewServer(grpc.UnaryInterceptor(
		limiter.UnaryServerInterceptor(limits, client.KeyFromMetadata("x-api-key"))))
	greeterv1.RegisterGreeterServiceServer(srv, greeter{})
	reflection.Register(srv) // so grpcurl works without the .proto

	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return fmt.Errorf("listen on GREETER_PORT: %w", err)
	}
	slog.Info("greeter listening", "addr", lis.Addr().String(), "limiter", limiterAddr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	stop() // a second signal now kills the process at once
	slog.Info("shutting down")
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(shutdownTimeout):
		srv.Stop()
	}
	if err := <-serveErr; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
