# distributed-rate-limiter

![CI](https://github.com/asmanya/distributed-rate-limiter/actions/workflows/ci.yml/badge.svg)

A rate limiter that runs as its own gRPC service, with Redis as the shared
store. It answers one question: may this key spend `cost` units right now?
Replicas keep no counters of their own, so a limit holds across all of them. A Go
SDK plugs it into a gRPC server or a `net/http` handler.

A demo instance runs on Cloud Run, see [Deploy to Cloud Run](#deploy-to-cloud-run).
Work in progress: the SDK is v0, and its API can change before v1.0.0.

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

Needs Go 1.27, Docker and grpcurl.

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
the same as the default `SHUTDOWN_TIMEOUT`, so the deploy sets it to 8 s.

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

## Deploy to Cloud Run

The service runs on Cloud Run with Upstash as the Redis. There is no CD:
`make deploy` builds the image from the working directory and deploys it.

### Live endpoint

```bash
grpcurl ratelimiter-748792177625.asia-south1.run.app:443 list
```

TLS ends at the Cloud Run edge on port 443. The container serves plain HTTP/2
(h2c) on `PORT`, which is why the service is deployed with `--use-http2`.

With `--min-instances 0` the first call after an idle period starts an instance.
One measurement, from a laptop with `grpcurl list` after about 20 idle minutes:
1.014 s for the cold call, 0.244 s for the next one. Both include the TLS
handshake and the reflection round trips, so the cold start itself is the
difference, about 0.77 s.

This is a demo endpoint: it may be throttled or offline. It has no
authentication, so anyone can call it. `AdminService` is not registered
(`ADMIN_ENABLED=false`), and the service is capped at 2 instances.

Cloud Run routes traffic to one port only, so `/metrics` on `METRICS_PORT` is not
reachable from outside. Logs go to Cloud Logging, where `severity` is read from
the JSON lines.

### What `make deploy` sets

| Setting | Value | Why |
| --- | --- | --- |
| Image | the last `Dockerfile` stage: distroless, non-root, 37.4 MB (size of the local image) | no shell, no package manager |
| `--use-http2` | on | gRPC needs HTTP/2 all the way to the container |
| `--max-instances` / `--min-instances` | 2 / 0 | caps cost and abuse; costs nothing when idle |
| `REDIS_URL` | from Secret Manager (`redis-url`) | it holds the password: never a plain env var value, never in the repo or the logs |
| `REDIS_TIMEOUT` | 200 ms | a starting value. From Cloud Run, an `Allow` took 4.7 to 9.4 ms in the 6 calls measured, so it is generous |
| `SHUTDOWN_TIMEOUT` | 8 s | Cloud Run sends `SIGKILL` 10 s after `SIGTERM` |
| `ADMIN_ENABLED` | `false` | `Reset` must not be reachable on a public endpoint |

The startup probe is a gRPC health check, set once on the service. `make deploy`
keeps it:

```bash
gcloud run services update ratelimiter --region asia-south1 \
  --startup-probe=grpc.port=8080,periodSeconds=5,timeoutSeconds=2,failureThreshold=6
```

There is no liveness probe on purpose: health does not depend on Redis, and a
restart would not fix a Redis outage.

### One-time setup

Needs `gcloud` logged in and `REDIS_URL` in the shell (for example
`set -a; . ./.env; set +a`). Replace `PROJECT_ID` and `PROJECT_NUMBER`.

```bash
gcloud config set project PROJECT_ID
gcloud services enable run.googleapis.com cloudbuild.googleapis.com \
  artifactregistry.googleapis.com secretmanager.googleapis.com

# The URL goes in through stdin, so it stays out of the shell history.
# printf, not echo: a trailing newline would become part of the URL.
printf %s "$REDIS_URL" | gcloud secrets create redis-url --data-file=-

gcloud iam service-accounts create ratelimiter-run
gcloud secrets add-iam-policy-binding redis-url \
  --member=serviceAccount:ratelimiter-run@PROJECT_ID.iam.gserviceaccount.com \
  --role=roles/secretmanager.secretAccessor

# Cloud Build runs as the default compute account and has to read the uploaded
# source. A new project does not grant that, and the first deploy fails with a 403.
gcloud projects add-iam-policy-binding PROJECT_ID \
  --member=serviceAccount:PROJECT_NUMBER-compute@developer.gserviceaccount.com \
  --role=roles/cloudbuild.builds.builder

make deploy    # the first run asks to create an Artifact Registry repository
```

Then set the startup probe once, with the command above.

Upstash was checked with the integration suite: the scripts, including the
`no-writes` flag that `Peek` uses, work there. Its free tier allows 500K commands
a month. Repeated runs of the suite used 474K of them, so run it against Upstash
once, not in a loop.

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
and a CPU profile showed no hot spot in the service's own code. The ceiling is
the Redis round trip. A hot key is no slower than 10,000 keys because Redis runs
every script on one thread anyway.

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
| `make load` | The benchmark matrix, about 15 minutes. Local Redis only |
| `make deploy` | Build and deploy to Cloud Run (see [Deploy to Cloud Run](#deploy-to-cloud-run)) |

Design decisions are recorded in [`docs/adr/`](docs/adr/).

## License

[MIT](LICENSE)
