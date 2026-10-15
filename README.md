# distributed-rate-limiter

Distributed rate limiting as a gRPC microservice — Go, Redis, Lua.

![CI](https://github.com/asmanya/distributed-rate-limiter/actions/workflows/ci.yml/badge.svg)

> Work in progress.

## Quickstart

You need Go 1.27, Docker and [grpcurl](https://github.com/fullstorydev/grpcurl).

```bash
git clone https://github.com/asmanya/distributed-rate-limiter
cd distributed-rate-limiter
make up    # Redis 8 in Docker
make run   # the service: gRPC on :8080, metrics on :9090
```

In another terminal, list what the server offers and call the `login` policy
(5 requests per 10 seconds) six times:

```bash
grpcurl -plaintext localhost:8080 list

for i in 1 2 3 4 5 6; do
  grpcurl -plaintext \
    -d '{"namespace":"demo","policy":"login","key":"user-1"}' \
    localhost:8080 ratelimiter.v1.RateLimitService/Allow
done
```

The first five answers have `"allowed": true` and a falling `remaining`. The
sixth has no `allowed` field (grpcurl leaves out `false`) and a `retryAfter`:

```json
{
  "limit": "5",
  "retryAfter": "3.785s",
  "resetAfter": "11.785s"
}
```

A denial is a normal `OK` response, not an error. Policies live in
[`deploy/policies.yaml`](deploy/policies.yaml); settings come from environment
variables, see [`.env.example`](.env.example). The API is in
[`api/ratelimiter/v1/`](api/ratelimiter/v1/) (`grpcurl describe` does not show
the comments, read the `.proto` files).

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
