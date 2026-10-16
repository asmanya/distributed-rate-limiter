// Command ratelimiter is the distributed rate limiter gRPC service. main only
// wires the pieces together; all behaviour lives in the internal packages.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/asmanya/distributed-rate-limiter/internal/config"
	"github.com/asmanya/distributed-rate-limiter/internal/limiter"
	"github.com/asmanya/distributed-rate-limiter/internal/metrics"
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

	log := newLogger(os.Stderr, cfg.LogLevel)
	log.Info("starting", "version", version, "config", cfg) // Config redacts the Redis password

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		// Load already validated the URL. The cause is dropped on purpose: a parse
		// error quotes the URL, and the URL holds the password.
		return errors.New("REDIS_URL is not a valid redis URL")
	}
	// Without these the REDIS_TIMEOUT budget exists only on paper:
	//  - ContextTimeoutEnabled: otherwise a context deadline never reaches the socket.
	//  - MaxRetries -1: 0 means "use the default of 3", -1 turns retries off. Four
	//    attempts would blow a 50ms budget.
	//  - DialTimeout and PoolTimeout bound a new connection and waiting for a free one.
	// PoolSize stays at the default until Phase 5 gives us data.
	opts.ContextTimeoutEnabled = true
	opts.MaxRetries = -1
	opts.DialTimeout = 2 * cfg.RedisTimeout
	opts.PoolTimeout = cfg.RedisTimeout
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

	m, err := metrics.New()
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	limiters := map[policy.Algorithm]limiter.Limiter{
		policy.TokenBucket:   limiter.NewInstrumented(limiter.NewTokenBucket(rdb), "tb", cfg.RedisTimeout, time.Now, m),
		policy.SlidingWindow: limiter.NewInstrumented(limiter.NewSlidingWindow(rdb), "sw", cfg.RedisTimeout, time.Now, m),
	}
	svc := server.Services{
		RateLimit: service.NewRateLimit(store, limiters, time.Now, log, m),
	}
	if cfg.AdminEnabled {
		svc.Admin = service.NewAdmin(store, limiters, time.Now, log)
		log.Warn("admin service is enabled; never do this on a public endpoint")
	}

	srv := server.New(svc, m, log)

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	// SIGHUP reloads the policy file. The channel is buffered because Notify drops
	// a signal it cannot deliver at once.
	reloadCtx, stopReload := context.WithCancel(context.Background())
	defer stopReload()
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)
	go watchReload(reloadCtx, sighup, func() error { return store.Reload(cfg.PolicyFile) }, log, m)

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

	// A server that stops on its own is a failure and takes the process down.
	// SIGTERM or SIGINT is a normal exit: drain, then return nil (exit 0).
	select {
	case err = <-errc:
		srv.Stop()
		return err
	case <-sigCtx.Done():
	}
	stop() // a second signal now kills the process at once instead of waiting

	log.Info("shutting down", "timeout", cfg.ShutdownTimeout)
	start := time.Now()
	// A fresh context: sigCtx is already done.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown forced", "error", err)
	}
	// The deferred Redis Close runs after this, once nothing can use it.
	log.Info("shutdown complete", "duration_ms", float64(time.Since(start).Microseconds())/1000)
	return nil
}

// watchReload reloads the policies each time a signal arrives, until sigs is
// closed or ctx ends. A failed reload is not fatal: Store.Reload swaps only a
// valid file, so the old policies keep serving. It is logged and counted instead.
func watchReload(ctx context.Context, sigs <-chan os.Signal, reload func() error, log *slog.Logger, m *metrics.Metrics) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-sigs:
			if !ok {
				return
			}
			if err := reload(); err != nil {
				log.Error("policy reload failed, keeping old policies", "error", err)
				m.PolicyReloads.WithLabelValues("failure").Inc()
				continue
			}
			log.Info("policies reloaded")
			m.PolicyReloads.WithLabelValues("success").Inc()
		}
	}
}

// stopped turns a server's return value into an error: a server that returns at
// all, even with nil, means the process can no longer do its job.
func stopped(what string, err error) error {
	if err == nil {
		return fmt.Errorf("%s stopped", what)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// newLogger builds the JSON logger. Cloud Logging reads "severity" and "message",
// not slog's "level" and "msg", so a severity filter works on Cloud Run.
func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.LevelKey:
				a.Key = "severity"
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		},
	}))
}
