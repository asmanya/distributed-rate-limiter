# distributed-rate-limiter

Distributed rate limiting as a gRPC microservice — Go, Redis, Lua.

![CI](https://github.com/asmanya/distributed-rate-limiter/actions/workflows/ci.yml/badge.svg)

> Work in progress.

## Quickstart

The demo stack runs two limiter replicas that share one Redis, and a small
service, the greeter, protected by the Go SDK. You need Docker and
[grpcurl](https://github.com/fullstorydev/grpcurl).

```bash
git clone https://github.com/asmanya/distributed-rate-limiter
cd distributed-rate-limiter
docker compose --profile demo up -d --build --wait
```

The first run downloads the Go image and builds two images, which took about a
minute here. Then call the greeter ten times with the same key. Its policy
allows 5 requests per 10 seconds per key:

```bash
for i in $(seq 1 10); do
  grpcurl -plaintext -H 'x-api-key: demo' -d '{"name":"world"}' \
    localhost:50051 examples.greeter.v1.GreeterService/SayHello
done
```

The first five answer `{"message": "Hello, world!"}`. The next five are refused:

```text
ERROR:
  Code: ResourceExhausted
  Message: rate limit exceeded
  Details:
  1)	{
    	  "@type": "type.googleapis.com/google.rpc.RetryInfo",
    	  "retryDelay": "1.899s"
    	}
```

`retryDelay` is how long to wait before trying again; it falls with every call
and the key is allowed again after a few seconds.

Both replicas answer these calls. The demo runs them with debug logging, so each
call shows up as a line, prefixed with the replica that handled it:

```bash
docker compose --profile demo logs ratelimiter | grep '"message":"rpc"'
```

How the calls split between the two replicas varies from run to run. The count
of allowed calls does not, because both replicas keep their state in the same
Redis: [`test/e2e`](test/e2e/e2e_test.go) checks that exactly the limit is
allowed when 1000 calls go through two servers.

Stop the demo with the command below. `make down` does not: it only stops the
services that are not part of the `demo` profile (Redis).

```bash
docker compose --profile demo down
```

## Using the SDK

A service that wants its callers limited uses the Go SDK,
[`pkg/client`](pkg/client/), on its server side. Create a client with the
limiter's address, a namespace that names your service, a fail mode and transport
credentials; the last three are required, so a plaintext connection or a
fail-open setup is always a choice made in your code. Install
`Client.UnaryServerInterceptor` on your gRPC server, or wrap an HTTP handler with
`Client.HTTPMiddleware`, and tell it which policy to apply and, with a `KeyFunc`,
what is being limited: `KeyFromMetadata` and `KeyFromHeader` read a request
value, `KeyFromPeer` uses the caller's IP address. A denied request gets
`ResourceExhausted` with a `RetryInfo` detail over gRPC, or `429` with
`Retry-After` over HTTP. When the limiter cannot answer, the fail mode decides
([ADR 0003](docs/adr/0003-fail-mode-in-caller.md)).
[`examples/greeter/main.go`](examples/greeter/main.go) is a complete small
service that does all of this.

Only unary gRPC calls are limited; streaming RPCs are not. The SDK is at v0: its
API can change before v1.0.0. Behind a proxy or load balancer, `KeyFromPeer` sees
the proxy's address and every user would share one key; use `KeyFromHeader` with
a header your proxy sets and overwrites.

## Run the service on its own

To run only the limiter, with Redis in Docker and the service on the host. You
need Go 1.27, Docker and grpcurl.

```bash
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

## Failure modes

What the service does when things go wrong. The commands and the raw output are
in [docs/chaos.md](docs/chaos.md); you can repeat them with
`bash scripts/chaos.sh`.

| What happens | What the service does |
| --- | --- |
| Redis is slow, paused or stopped | `Allow` returns `Unavailable` after `REDIS_TIMEOUT` (50 ms by default); measured at about 51 ms. The process stays up and health stays `SERVING` |
| Redis comes back | Requests succeed again without restarting the service |
| The caller's own deadline expires or it cancels | `DeadlineExceeded` or `Canceled`, not `Unavailable`: that is not a store outage |
| A handler panics | The caller gets `Internal`, the stack goes to the log, the process keeps serving |
| `SIGTERM` | Health goes `NOT_SERVING`, requests in flight finish, then exit 0. After `SHUTDOWN_TIMEOUT` anything left is stopped by force |
| `SIGHUP` with a broken policy file | The old policies keep serving; the failure is logged and counted |
| Redis is down at startup | The service starts anyway and logs a warning |

A denied request is not a failure: it is an `OK` response with `allowed=false`.

Whether to let traffic through (fail open) or reject it (fail closed) when the
limiter cannot answer is the caller's decision, not the service's
([ADR 0003](docs/adr/0003-fail-mode-in-caller.md)). The Go SDK carries that
choice: it has no default fail mode, and a client cannot be created without one
(see [Using the SDK](#using-the-sdk)).

Two things to know when running it on a platform:

- Policies reload on `SIGHUP`. A platform that cannot send your process a signal
  (Cloud Run, for one) needs a new deploy to change a policy. Reload is meant
  for a VM, or Kubernetes with a mounted ConfigMap.
- Cloud Run allows 10 seconds between `SIGTERM` and `SIGKILL`, and
  `SHUTDOWN_TIMEOUT` defaults to 10 seconds. Set it lower there.

## Observability

Metrics are served on `METRICS_PORT` (9090) at `/metrics`, together with the Go
runtime and process metrics.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `grpc_server_handled_total` | counter | `method`, `code` | RPCs finished, by result |
| `grpc_server_handling_seconds` | histogram | `method` | RPC latency |
| `ratelimiter_decisions_total` | counter | `policy`, `result` | `Allow` decisions: `allowed` or `denied` |
| `ratelimiter_redis_duration_seconds` | histogram | `script` | Redis round trip, buckets from 0.5 ms to 250 ms |
| `ratelimiter_redis_errors_total` | counter | `script` | Redis failures |
| `ratelimiter_policy_reloads_total` | counter | `result` | Reloads: `success` or `failure` |

Labels only ever hold a small fixed set of values. The request key, a user id
or an IP address is never a label, because every distinct value would become a
new time series. Health checks are not counted.

Logs are JSON on stderr, with `severity` and `message` fields so Cloud Logging
reads them. There is one line per RPC with the method, code, duration and peer;
successful calls log at debug, so with the default `LOG_LEVEL=info` you see
only problems. The request key is never logged. Lines written by the Redis
client go through the same logger.
