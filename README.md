# distributed-rate-limiter

![CI](https://github.com/asmanya/distributed-rate-limiter/actions/workflows/ci.yml/badge.svg)

A rate limiter that runs as its own gRPC service, with Redis as the shared
store. It answers one question: may this key spend `cost` units right now?
Replicas keep no counters of their own, so a limit holds across all of them. A Go
SDK plugs it into a gRPC server or a `net/http` handler.

Work in progress. The SDK is v0, and its API can change before v1.0.0.

## How it works

- Every decision is one Lua script in Redis. Read, decide and write happen in a
  single step, so two replicas can never both hand out the last unit
  ([ADR 0001](docs/adr/0001-lua-atomicity.md)).
- The service passes the current time into the script. Redis `TIME` is not
  used, which keeps scripts deterministic and lets tests control the clock
  ([ADR 0002](docs/adr/0002-time-source.md)).
- A denied request is a normal `OK` response with `allowed=false`, and it writes
  nothing to Redis.
- If Redis fails or takes longer than `REDIS_TIMEOUT`, the service returns
  `Unavailable`. Whether to let the request through is the caller's decision,
  not the service's ([ADR 0003](docs/adr/0003-fail-mode-in-caller.md)).
- Keys are `rl:{namespace:policy:key}:tb` and `rl:{namespace:policy:key}:sw:<window>`.
  The braces are a Redis Cluster hash tag: all keys of one limiter land in one
  slot.

## Quickstart

Two limiter replicas sharing one Redis, and a demo service (`greeter`)
protected by the SDK. Needs Docker and
[grpcurl](https://github.com/fullstorydev/grpcurl).

```bash
git clone https://github.com/asmanya/distributed-rate-limiter
cd distributed-rate-limiter
docker compose --profile demo up -d --build --wait
```

The greeter allows 5 calls per 10 seconds per API key. Call it ten times:

```bash
for i in $(seq 1 10); do
  grpcurl -plaintext -H 'x-api-key: demo' -d '{"name":"world"}' \
    localhost:50051 examples.greeter.v1.GreeterService/SayHello
done
```

The first five return `{"message": "Hello, world!"}`. The rest are refused:

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

Both replicas serve these calls. The demo runs them at debug level, so each
logs one line per call:

```bash
docker compose --profile demo logs ratelimiter | grep '"message":"rpc"'
```

The split between replicas changes from run to run. The number of allowed calls
does not: [`test/e2e`](test/e2e/e2e_test.go) sends 1000 calls through two
servers and checks that exactly the limit is allowed.

Stop it with `docker compose --profile demo down`. `make down` stops only Redis.

## API

Contracts are in [`api/ratelimiter/v1/`](api/ratelimiter/v1/). Server
reflection is on, so grpcurl works without the `.proto` files.

| RPC | What it does |
| --- | --- |
| `RateLimitService.Allow` | Decides, and if allowed, takes `cost` units from the key |
| `RateLimitService.Peek` | Returns the decision `Allow` would make, without changing anything |
| `AdminService.Reset` | Clears one key's state. Registered only with `ADMIN_ENABLED=true`. Never expose it |

A request carries `namespace` (the calling service, e.g. `orders-api`),
`policy` (a name from the policies file), `key` (a user id, API key or IP) and
`cost` (0 means 1). The response has `allowed`, `limit`, `remaining`,
`retry_after` and `reset_after`.

| Situation | Code |
| --- | --- |
| A decision was made, allowed or not | `OK` |
| Bad name or key, negative cost, cost above the limit | `InvalidArgument` |
| Unknown policy | `NotFound` |
| Redis error or timeout | `Unavailable` |
| The caller's deadline passed, or it cancelled | `DeadlineExceeded`, `Canceled` |
| A bug (panic) in the handler | `Internal` |

Namespace and policy must be non-empty and contain no `:`, `{` or `}`. The key
must be 1 to 256 bytes with no `{` or `}`.

## Go SDK

[`pkg/client`](pkg/client/) is used on the server side of the service being
protected. From [`examples/greeter`](examples/greeter/main.go):

```go
limiter, err := client.New("dns:///localhost:8080",
	client.WithNamespace("greeter"),
	client.WithFailMode(client.FailOpen),
	client.WithTransportCredentials(insecure.NewCredentials()), // TLS in production
)
if err != nil {
	return err
}
defer limiter.Close()

limits := map[string]string{greeterv1.GreeterService_SayHello_FullMethodName: "greeter"}
srv := grpc.NewServer(grpc.UnaryInterceptor(
	limiter.UnaryServerInterceptor(limits, client.KeyFromMetadata("x-api-key"))))
```

For HTTP: `limiter.HTTPMiddleware("search", client.KeyFromHeader("X-API-Key"))(handler)`.

- Namespace, fail mode and credentials have no defaults. Failing open and
  running without TLS are choices your code makes explicitly.
- The key comes from `KeyFromMetadata`, `KeyFromHeader` or `KeyFromPeer` (the
  caller's IP). If the value is missing, the SDK uses the caller's IP. A key the
  service would reject is hashed, so a malformed key cannot bypass the limit.
- Denied: `ResourceExhausted` with `RetryInfo`, or HTTP `429` with
  `Retry-After` in whole seconds.
- Limiter unreachable: `FailOpen` lets the request through. `FailClosed`
  returns `Unavailable`, or HTTP `503`.
- Each call has a 100 ms timeout by default (`WithTimeout`). Keep it above the
  service's `REDIS_TIMEOUT` plus the network.
- Only unary RPCs are limited. Streaming RPCs pass through.
- Behind a proxy, `KeyFromPeer` sees the proxy, and every user shares one key.
  Use `KeyFromHeader` with a header the proxy sets and overwrites.

## Running the service

Needs Go 1.27 and Docker.

```bash
make up    # Redis 8 in Docker
make run   # gRPC on :8080, metrics on :9090; reads .env if present
```

```bash
grpcurl -plaintext -d '{"namespace":"demo","policy":"login","key":"user-1"}' \
  localhost:8080 ratelimiter.v1.RateLimitService/Allow
```

The `login` policy allows 5 per 10 seconds. The sixth call within 10 seconds
returns no `allowed` field (grpcurl omits `false`) and a `retryAfter`.

### Configuration

Environment variables only. Invalid values stop startup, with every problem
listed at once. A template is in [`.env.example`](.env.example).

| Variable | Default | Notes |
| --- | --- | --- |
| `PORT` | `8080` | gRPC |
| `METRICS_PORT` | `9090` | HTTP, `/metrics`. Must differ from `PORT` |
| `REDIS_URL` | `redis://localhost:6379/0` | `rediss://` for TLS. Only the host is logged |
| `REDIS_TIMEOUT` | `50ms` | Per Redis call, 1ms to 1s |
| `POLICY_FILE` | `deploy/policies.yaml` | |
| `ADMIN_ENABLED` | `false` | Registers `AdminService` |
| `PPROF_ENABLED` | `false` | Serves `/debug/pprof/` on `METRICS_PORT`. Not for production |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `SHUTDOWN_TIMEOUT` | `10s` | How long in-flight requests get after `SIGTERM` |

### Policies

```yaml
policies:
  - name: search
    algorithm: token_bucket    # or sliding_window
    limit: 100
    period: 1m
```

Unknown fields are rejected. `SIGHUP` reloads the file. If the new file is
invalid, the old policies stay in effect and the failure is logged and counted.

## Algorithms

Both use O(1) memory per key.

| | Token bucket | Sliding window counter |
| --- | --- | --- |
| State | One hash: tokens left, last update | Two counters: this window and the previous one |
| Behaviour | Bursts up to `limit`, then refills at `limit` per `period` | At most `limit` per window, with the previous window weighted in |
| Accuracy | Exact | Approximate: assumes the previous window was evenly spread |
| Use for | General APIs with bursts | Strict quotas, e.g. 5 logins per 10 seconds |

The sliding window prevents the fixed-window double burst at a boundary
([test](internal/limiter/slidingwindow_test.go)).
[`test/load/accuracy_test.go`](test/load/accuracy_test.go) sends 2000 requests
from 200 goroutines, over one and two Redis clients, against a limit of 500.
Both algorithms allow exactly 500.

## Failure modes

Reproduced by [`scripts/chaos.sh`](scripts/chaos.sh). Raw output is in
[docs/chaos.md](docs/chaos.md).

| Event | Behaviour |
| --- | --- |
| Redis slow, paused or stopped | `Unavailable` after `REDIS_TIMEOUT` (measured about 51 ms with 50 ms). The process stays up and health stays `SERVING` |
| Redis comes back | Requests succeed again, no restart |
| Redis down at startup | Starts anyway and logs a warning |
| Caller's deadline expires or it cancels | `DeadlineExceeded` or `Canceled`, not `Unavailable` |
| Handler panics | `Internal` to the caller, stack in the log, process keeps serving |
| `SIGTERM` | Health goes `NOT_SERVING`, in-flight requests finish, exit 0. After `SHUTDOWN_TIMEOUT` the rest are stopped |
| `SIGHUP` with a broken policy file | Old policies keep serving |

Health never depends on Redis. A Redis outage should not make the platform
restart every replica.

On platforms that cannot send `SIGHUP` (Cloud Run, for one), changing a policy
needs a redeploy. Cloud Run gives 10 seconds between `SIGTERM` and `SIGKILL`,
the same as the default `SHUTDOWN_TIMEOUT`, so set it lower there.

## Observability

Prometheus metrics on `METRICS_PORT` at `/metrics`, plus the Go runtime and
process metrics.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `grpc_server_handled_total` | counter | `method`, `code` | RPCs finished |
| `grpc_server_handling_seconds` | histogram | `method` | RPC latency |
| `ratelimiter_decisions_total` | counter | `policy`, `result` | `allowed` or `denied` |
| `ratelimiter_redis_duration_seconds` | histogram | `script` | Redis round trip, 0.5 ms to 250 ms buckets |
| `ratelimiter_redis_errors_total` | counter | `script` | Redis failures |
| `ratelimiter_policy_reloads_total` | counter | `result` | `success` or `failure` |

No label ever holds a request key, user id or IP address. Health checks are not
counted.

Logs are JSON on stderr, with `severity` and `message` fields for Cloud
Logging. One line per RPC with method, code, duration and peer. Successful
calls log at debug, so at the default `info` you only see problems. Request
keys are never logged.

## Benchmarks

Median of 3 runs, 30 s each, with `ghz` over one gRPC connection against a
token bucket policy, Redis 8 in Docker. ghz, the service and Redis all ran on
one AMD Ryzen 5 5600H laptop inside WSL2.

| Scenario | RPS | p50 | p99 |
| --- | --- | --- | --- |
| 10,000 keys, 10 concurrent clients | 7,623 | 1.08 ms | 2.05 ms |
| 10,000 keys, 50 concurrent clients | 15,868 | 2.58 ms | 5.95 ms |
| 10,000 keys, 100 concurrent clients | 18,696 | 4.41 ms | 9.76 ms |
| One hot key, 100 concurrent clients | 20,140 | 4.21 ms | 9.30 ms |

With 10,000 keys, going from 50 to 100 clients raised RPS by 18 % and p50 by
71 %. At 100 clients Redis was at 89 to 93 % of one core (one sample per run),
and a CPU profile showed no hot spot in the service's own code. The ceiling is the Redis round trip. A hot key is no
slower than 10,000 keys because Redis runs every script on one thread anyway.

One machine, no network between the parts. Hosted Redis, Redis Cluster and the
sliding window were not measured. Method, raw output and analysis:
[docs/benchmarks.md](docs/benchmarks.md).

## Development

| Command | |
| --- | --- |
| `make tools` | Install the pinned tools (`go install tool`) |
| `make proto` | Lint the `.proto` files and regenerate `gen/` |
| `make lint` | `golangci-lint` and `buf lint` |
| `make test` | Unit tests, with `-race` |
| `make test-integration` | Integration tests against a real Redis. Run `make up` and export `REDIS_URL` first, or they skip |
| `make load` | The benchmark matrix, about 15 minutes |

Design decisions are recorded in [`docs/adr/`](docs/adr/).

## License

[MIT](LICENSE)
