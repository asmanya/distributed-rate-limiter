// Command ratelimiter is the distributed rate limiter gRPC service. main only
// wires the pieces together; all behaviour lives in the internal packages.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/asmanya/distributed-rate-limiter/internal/config"
	"github.com/asmanya/distributed-rate-limiter/internal/limiter"
	"github.com/asmanya/distributed-rate-limiter/internal/policy"
	"github.com/asmanya/distributed-rate-limiter/internal/server"
	"github.com/asmanya/distributed-rate-limiter/internal/service"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ratelimiter:", err)
		os.Exit(1)
	}
}

func run() error {
	// Config and policy problems are fatal: a restart cannot fix a wrong
	// setting, so failing fast with every problem listed is the useful thing.
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	log.Info("starting", "version", version, "config", cfg) // Config redacts the Redis password

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		// Load already validated the URL. The cause is dropped on purpose: a parse
		// error quotes the URL, and the URL holds the password.
		return errors.New("REDIS_URL is not a valid redis URL")
	}
	rdb := redis.NewClient(opts)
	defer func() { _ = rdb.Close() }()

	// Redis being down is NOT fatal. A dependency that is down does not mean the
	// process is broken: requests will succeed as soon as Redis is back, and
	// exiting here would only put the platform into a restart loop.
	pingCtx, cancel := context.WithTimeout(context.Background(), cfg.RedisTimeout)
	pingErr := rdb.Ping(pingCtx).Err()
	cancel()
	if pingErr != nil {
		log.Warn("redis not reachable at startup; starting anyway",
			"redis_host", cfg.RedisHost(), "error", pingErr)
	}

	store := policy.NewStore()
	if err := store.Reload(cfg.PolicyFile); err != nil {
		return fmt.Errorf("load policies: %w", err)
	}

	limiters := map[policy.Algorithm]limiter.Limiter{
		policy.TokenBucket:   limiter.NewTokenBucket(rdb),
		policy.SlidingWindow: limiter.NewSlidingWindow(rdb),
	}
	svc := server.Services{
		RateLimit: service.NewRateLimit(store, limiters, time.Now, log),
	}
	if cfg.AdminEnabled {
		svc.Admin = service.NewAdmin(store, limiters, time.Now, log)
		log.Warn("admin service is enabled; never do this on a public endpoint")
	}

	metrics, err := server.RuntimeMetrics()
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	srv := server.New(svc, metrics)

	grpcLis, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.Port))
	if err != nil {
		return fmt.Errorf("listen on PORT: %w", err)
	}
	metricsLis, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.MetricsPort))
	if err != nil {
		return fmt.Errorf("listen on METRICS_PORT: %w", err)
	}

	errc := make(chan error, 2)
	go func() { errc <- stopped("grpc server", srv.ServeGRPC(grpcLis)) }()
	go func() { errc <- stopped("metrics server", srv.ServeMetrics(metricsLis)) }()
	log.Info("listening", "grpc", grpcLis.Addr().String(), "metrics", metricsLis.Addr().String())

	// Whichever server stops first takes the process down with it. Signals and
	// graceful shutdown come in Phase 3.
	err = <-errc
	srv.Stop()
	return err
}

// stopped turns a server's return value into an error: a server that returns at
// all, even with nil, means the process can no longer do its job.
func stopped(what string, err error) error {
	if err == nil {
		return fmt.Errorf("%s stopped", what)
	}
	return fmt.Errorf("%s: %w", what, err)
}
