// Command ratelimiter is the distributed rate limiter gRPC service.
package main

import "log/slog"

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	unused := 1
	slog.Info("ratelimiter", "version", version)
}
