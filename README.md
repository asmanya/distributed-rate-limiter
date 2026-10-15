# distributed-rate-limiter

Distributed rate limiting as a gRPC microservice — Go, Redis, Lua.

![CI](https://github.com/asmanya/distributed-rate-limiter/actions/workflows/ci.yml/badge.svg)

> Work in progress.

## Algorithms

Two algorithms are implemented in `internal/limiter`. Both keep O(1) memory per
key.

| | Token bucket | Sliding window counter |
| --- | --- | --- |
| State in Redis | One hash: tokens left, last update time | Two counters: current and previous window |
| Behaviour | Allows a burst up to capacity, then refills at a steady rate | At most `limit` per window; the previous window still counts, in proportion |
| Accuracy | Exact | An approximation (see below) |
| Fits | General APIs, e.g. 100 requests per minute with bursts | Strict quotas, e.g. 5 logins per 10 seconds |

Every decision is a single Lua script run inside Redis, so concurrent callers
cannot interleave between the read and the write
([ADR 0001](docs/adr/0001-lua-atomicity.md)). The proof is
[`accuracy_test.go`](test/load/accuracy_test.go): 2000 attempts from 200
goroutines, with one and with two Redis clients, against a limit of 500 allow
exactly 500, for both algorithms.

The sliding window avoids the fixed-window double burst: ten requests just
before a window boundary and ten just after are not both allowed
([test](internal/limiter/slidingwindow_test.go)). It is still an approximation,
because the previous window is assumed to be evenly spread.
