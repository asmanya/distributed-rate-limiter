//go:build integration

package limiter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// testClient connects to the Redis named by REDIS_URL. It skips, not fails,
// when the variable is unset so a plain `go test` stays usable without Redis.
func testClient(t testing.TB) *redis.Client {
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

// uniqueNamespace gives every test its own key space, so parallel tests (and
// leftovers from earlier runs) never see each other's state. We never FLUSHALL:
// the Redis may hold someone's other data.
func uniqueNamespace(t testing.TB) string {
	t.Helper()
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(t.Name()))
	if len(name) > 50 { // namespaces are capped at 64 bytes
		name = name[:50]
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return name + "-" + hex.EncodeToString(b)
}
