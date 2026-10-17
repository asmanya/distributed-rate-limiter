//go:build integration

// Package e2e_test checks the property the whole project is for: several limiter
// servers, one Redis, and the limit still holds across all of them. It runs
// in-process, so it is fast and deterministic enough for CI; the compose stack
// shows the same thing to a person.
package e2e_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"

	"github.com/asmanya/distributed-rate-limiter/internal/limiter"
	"github.com/asmanya/distributed-rate-limiter/internal/metrics"
	"github.com/asmanya/distributed-rate-limiter/internal/policy"
	"github.com/asmanya/distributed-rate-limiter/internal/server"
	"github.com/asmanya/distributed-rate-limiter/internal/service"
	"github.com/asmanya/distributed-rate-limiter/pkg/client"
)

const (
	policyLimit = 100
	goroutines  = 200
	callsEach   = 5 // 200 * 5 = 1000 attempts against a limit of 100
)

// fixedNow never moves. With a real clock, tokens would refill during the test
// and the count could pass the limit even though the code is right.
var fixedNow = time.UnixMilli(1_700_000_040_000)

// countingLimiter counts the Allow calls a server receives, so the test can show
// that both servers took traffic.
type countingLimiter struct {
	limiter.Limiter
	calls atomic.Int64
}

func (c *countingLimiter) Allow(ctx context.Context, r limiter.Request) (limiter.Decision, error) {
	c.calls.Add(1)
	return c.Limiter.Allow(ctx, r)
}

// policies is an in-memory service.PolicySource.
type policies map[string]policy.Policy

func (p policies) Get(name string) (policy.Policy, bool) {
	v, ok := p[name]
	return v, ok
}

func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; run `make up` and export it")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	return rdb
}

// uniqueName keeps one run's keys apart from every other run's, so the test needs
// no cleanup and never flushes Redis.
func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return prefix + hex.EncodeToString(b)
}

// startServer runs one real limiter server, on a random port, against the shared
// Redis. It returns the address and the counter of the calls it receives.
func startServer(t *testing.T, rdb *redis.Client, alg policy.Algorithm, pol policies) (string, *countingLimiter) {
	t.Helper()
	var real limiter.Limiter
	switch alg {
	case policy.TokenBucket:
		real = limiter.NewTokenBucket(rdb)
	case policy.SlidingWindow:
		real = limiter.NewSlidingWindow(rdb)
	default:
		t.Fatalf("unknown algorithm %q", alg)
	}
	counting := &countingLimiter{Limiter: real}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := metrics.New()
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	svc := service.NewRateLimit(pol,
		map[policy.Algorithm]limiter.Limiter{alg: counting},
		func() time.Time { return fixedNow }, log, m)
	srv := server.New(server.Services{RateLimit: svc}, m, log)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.ServeGRPC(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), counting
}

// newSDK builds the real SDK client, which learns both addresses from a manual
// resolver. A manual resolver serves one client only, so each call gets a scheme
// of its own.
func newSDK(t *testing.T, addrs []string, namespace string) *client.Client {
	t.Helper()
	scheme := uniqueName(t, "e2e")
	b := manual.NewBuilderWithScheme(scheme)
	state := resolver.State{}
	for _, a := range addrs {
		state.Addresses = append(state.Addresses, resolver.Address{Addr: a})
	}
	b.InitialState(state)
	resolver.Register(b)

	c, err := client.New(scheme+":///limiter",
		client.WithNamespace(namespace),
		// FailClosed: a limiter error must show up as an error, never turn into
		// an "allowed" that would hide a wrong count.
		client.WithFailMode(client.FailClosed),
		// Generous: 1000 calls under -race must not time out, because a call that
		// timed out after the server counted it would blur the exact count.
		client.WithTimeout(5*time.Second),
		client.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestGlobalLimitAcrossTwoServers sends 1000 calls for one key through the SDK to
// two limiter servers that share one Redis, and checks that exactly the limit is
// allowed and that both servers took part.
func TestGlobalLimitAcrossTwoServers(t *testing.T) {
	tests := []struct {
		name string
		alg  policy.Algorithm
	}{
		{"token bucket", policy.TokenBucket},
		{"sliding window", policy.SlidingWindow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rdb := redisClient(t)
			pol := policies{
				"e2e":    {Name: "e2e", Algorithm: tt.alg, Limit: policyLimit, Period: time.Hour},
				"warmup": {Name: "warmup", Algorithm: tt.alg, Limit: 1_000_000, Period: time.Hour},
			}
			addrA, a := startServer(t, rdb, tt.alg, pol)
			addrB, b := startServer(t, rdb, tt.alg, pol)
			c := newSDK(t, []string{addrA, addrB}, uniqueName(t, "e2e-"))
			ctx := context.Background()

			// Round robin starts on whichever connection is ready first. Warm up on
			// a policy of its own until both servers have taken a call, so the real
			// run does not depend on how fast the second connection comes up.
			warmed := false
			for i := 0; i < 500 && !warmed; i++ {
				if _, err := c.Allow(ctx, "warmup", "w"); err != nil {
					t.Fatalf("warm-up Allow: %v", err)
				}
				warmed = a.calls.Load() > 0 && b.calls.Load() > 0
			}
			if !warmed {
				t.Fatalf("after 500 warm-up calls the servers got %d and %d: no round robin", a.calls.Load(), b.calls.Load())
			}
			beforeA, beforeB := a.calls.Load(), b.calls.Load()

			var wg sync.WaitGroup
			var allowed, denied, errs atomic.Int64
			for g := 0; g < goroutines; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < callsEach; i++ {
						d, err := c.Allow(ctx, "e2e", "user-1")
						switch {
						case err != nil:
							errs.Add(1)
							t.Errorf("Allow: %v", err)
							return
						case d.Allowed:
							allowed.Add(1)
						default:
							denied.Add(1)
						}
					}
				}()
			}
			wg.Wait()

			const attempts = goroutines * callsEach
			if errs.Load() != 0 {
				t.Fatalf("%d calls failed", errs.Load())
			}
			if got := allowed.Load(); got != policyLimit {
				t.Errorf("allowed = %d, want exactly %d", got, policyLimit)
			}
			if got := allowed.Load() + denied.Load(); got != attempts {
				t.Errorf("allowed + denied = %d, want %d", got, attempts)
			}
			gotA, gotB := a.calls.Load()-beforeA, b.calls.Load()-beforeB
			if gotA == 0 || gotB == 0 {
				t.Errorf("traffic reached only one server: A got %d, B got %d", gotA, gotB)
			}
			if gotA+gotB != attempts {
				t.Errorf("servers saw %d + %d calls, want %d in all", gotA, gotB, attempts)
			}
			t.Logf("allowed %d of %d; server A took %d calls, server B took %d", allowed.Load(), attempts, gotA, gotB)
		})
	}
}
