# Benchmarks

How fast `Allow` is under load, and *why* it is that fast and no faster. Every
number below comes from the raw `ghz` output in [`raw/`](benchmarks/raw/),
printed by [`scripts/load.sh`](../scripts/load.sh). Values marked *derived* are
arithmetic on those numbers, not separate measurements.

| | |
| --- | --- |
| Run | 2026-10-17, commit `88472d6` (`raw/meta.txt`) |
| Machine | AMD Ryzen 5 5600H (6 cores / 12 threads), laptop on charger |
| OS | Windows host, WSL2 (Ubuntu 24.04.3, kernel 6.18.33.2), 7.4 GiB RAM for the VM |
| Versions | Go 1.27.1, Redis 8.10.2 (`redis:8` in Docker 29.1.3), ghz v0.121.0 |
| Service | `REDIS_TIMEOUT=50ms`, `LOG_LEVEL=warn`, go-redis pool size default |
| Load | ghz, 1 gRPC connection, 10 s warm-up (discarded), 30 s measured, 3 repeats |

The runs were made at `88472d6`. Later commits changed `cmd`,
`internal/config`, `internal/server` (an optional pprof endpoint, off by
default) and comments in `pkg/client`. `internal/limiter` and `internal/service`,
the request path, are unchanged since.

**Where this runs matters.** Docker runs inside the WSL2 VM, and the service
reaches Redis at `localhost:6379` through a port-forward hop. Everything (ghz,
service, Redis) shares the same 12 threads. On native Linux, or with Redis on
its own host, the numbers will be different. Treat them as relative, not as a
promise for production.

## Floor: how fast can one call be

| Measurement | Result |
| --- | --- |
| `redis-cli --latency` from the host (PING) | 0.625 ms avg (min 0.264, max 1.353) |
| `redis-cli --latency` inside the container | 0.284 ms avg |
| Go benchmark, one sequential token bucket `Allow` straight to Redis (Step 1.11) | 0.324 ms median |

The service runs on the host, so its floor is the host-side round trip plus
script execution. No `Allow` through the service can be faster than this.

## Scenarios

| Scenario | Policy | Key | What it measures |
| --- | --- | --- | --- |
| A, spread | `load_open` (token bucket, 10⁹ per second: never denies) | 10,000 random keys | normal multi-user traffic |
| B, hot key | `load_open` | one fixed key | single-key contention |
| C, deny path | `load_hot` (token bucket, 100 per minute) | one fixed key | the deny path, which writes nothing |

Each scenario ran at concurrency 10, 50 and 100 (C only at 50). B and C use a
fresh key per run (and another one for the warm-up), so repeats do not share
state. A's 10,000 keys are shared between runs, which is harmless because
`load_open` never denies.

## Results

Median of 3 runs. Each column's median is taken on its own, so one row can mix
runs (for example, RPS from r2 and p99 from r3).

| Run | RPS | p50 ms | p95 ms | p99 ms | non-OK | service CPU % (of 1 core) | Redis CPU % |
| --- | --- | --- | --- | --- | --- | --- | --- |
| A10 | 7623 | 1.08 | 1.64 | 2.05 | 6 | 237.4 | 62.83 |
| A50 | 15868 | 2.58 | 4.60 | 5.95 | 38 | 278.3 | 83.36 |
| A100 | 18696 | 4.41 | 7.81 | 9.76 | 97 | 274.2 | 88.71 |
| B10 | 7893 | 1.08 | 1.62 | 2.01 | 7 | 242.0 | 62.85 |
| B50 | 16963 | 2.49 | 4.36 | 5.62 | 19 | 293.6 | 83.64 |
| B100 | 20140 | 4.21 | 7.39 | 9.30 | 97 | 292.0 | 92.69 |
| C50 | 17654 | 2.36 | 4.20 | 5.49 | 31 | 296.8 | 75.01 |

- **Service CPU** is user + system time of the service process over the
  measured 30 s (`/proc/<pid>/stat`). 100 % = one core.
- **Redis CPU** is a single `docker stats` sample taken halfway through the
  run, not an average. 100 % = one core, which is the ceiling for Redis
  scripts: Redis executes commands on one main thread.

## Interpretation

### 1. p50 is close to the Redis round trip: only partly

At concurrency 10, p50 is 1.08 ms: 1.7× the `redis-cli` floor and 3.3× the
sequential Go `Allow` (*derived*). The Redis round trip is the largest single
piece, but not all of it: the request also crosses a gRPC hop (ghz → service)
and the service itself. At concurrency 50, p50 is already 4.1× the floor
(*derived*): from there on, waiting in a queue costs more than the network.

There is no concurrency-1 run, so the latency with no queueing at all is not
measured here.

### 2. Hot key (B) slower than spread (A): no

B was never slower. B's RPS is above A's at every concurrency: +3.5 % at 10,
+6.9 % at 50, +7.7 % at 100 (*derived*). Redis CPU at the same concurrency is
about the same (62.83 vs 62.85, 83.36 vs 83.64, 88.71 vs 92.69).

Why: Redis runs *every* script on one thread, whatever the key. 10,000 keys
are executed one after another just like one key is, so on a single Redis
instance a hot key costs nothing extra. The limit is the instance, not the
key. A hot key becomes a problem with Redis Cluster: one key lives on one
shard, so adding shards spreads A's traffic but not B's.

Why A is a little slower is not confirmed. Two candidates:

- **Key churn in Redis.** A bucket's TTL is the time to full refill, and with
  `load_open` that rounds up to 1 ms. B's one key is hit thousands of times a
  millisecond and never expires. A's keys are each hit about 1.9 times a second
  (*derived*: 18,696 RPS / 10,000 keys), so nearly every A request finds its key
  expired and creates it again, and Redis keeps expiring keys in the
  background. `INFO stats` (`expired_keys`) during a run would confirm it.
- **Client cost.** ghz renders the random-key template on every request.

### 3. p99 jumps with concurrency: yes, together with p50

p99 goes 2.05 → 5.95 → 9.76 ms for A (2.01 → 5.62 → 9.30 for B). But p50 moves
with it, and p99/p50 stays between 1.86 and 2.31 at every level (*derived*). The
whole distribution shifts. This is queueing, not a tail-only problem.

Throughput stops scaling well before latency does:

| A | 10 → 50 | 50 → 100 |
| --- | --- | --- |
| RPS | ×2.08 | +17.8 % |
| p50 | ×2.39 | +70.9 % |

(B is the same shape: ×2.15 / +18.7 % RPS, ×2.31 / +69.1 % p50; *derived*.)
Past concurrency 50, extra in-flight requests mostly add waiting time.

Not confirmed: whether the wait is in the go-redis pool or in Redis. Pool stats
were not recorded. At concurrency 100, the Redis CPU medians were 88.71 % (A)
and 92.69 % (B). That is close to its one-core ceiling, which makes Redis the
likelier place.

### 4. Is the bottleneck in the service: not in its own code

The case where "Redis CPU is low but throughput is flat" did not happen: Redis
CPU was 58–93 % across all runs. Still, the service used 2.4–3.0 cores
throughout, which is about 145–311 µs of CPU per request (*derived*). It was
311 µs at concurrency 10 and 145 µs at 100. ghz ran on the same 12 threads, and
its CPU was not recorded, so it cannot be ruled out as a limit.

A 30 s CPU profile was taken during one A100 run, with `PPROF_ENABLED=true`
(that run gave 18,106 RPS and p99 12.11 ms; profiling has its own cost, and it
is a single run). The service used 72.26 s of CPU in 30 s, 2.4 cores.

| Where the CPU went | Share |
| --- | --- |
| `Script.EvalSha` (go-redis, the Redis round trip), cumulative | 41 % |
| `net.(*conn).Write` to Redis, cumulative | 17 % |
| syscalls (`Syscall6`), flat | 23.8 % |
| scheduler and `futex`, flat | 10.8 % (futex) |
| goroutine stack growth (`pcvalue`, `adjustframe`, `getStackMap`) | about 6 % |
| `mallocgc`, cumulative | 5.6 % |
| logging, metrics and recovery interceptors, flat each | 0.1 to 0.2 % |

The service's own code does not show up. Its CPU goes into talking to Redis
(one round trip per decision, by design) and into the Go runtime. Redis was at
88.71 % (A100) and 92.69 % (B100) of one core in the snapshot, so cutting the
service's CPU would not raise throughput much. No fix was made. Ideas left for
later: reuse gRPC handler goroutines to cut stack growth (about 6 %, gain
unmeasured), and batching Redis calls, which would change the one-decision,
one-script design and is not planned.

### 5. Errors under load: not Redis

Every run had a few non-OK results (at most 100 out of 560,889, *derived*
0.018 %). They are not Redis timeouts:

| Source | What it says |
| --- | --- |
| ghz, all 21 runs | 781 × `Unavailable: ... use of closed network connection`, 13 × `Canceled: the client connection is closing`, 1 × `Unavailable: transport is closing` |
| service log | 113 lines, all `code=Canceled`, zero `Unavailable` (the service logs `Unavailable` at warn, and the level was warn) |

Every run ended on ghz's 30 s timer (`endReason: timeout`). When ghz stops, it
closes the connection with requests still in flight. That is why the non-OK
count grows with concurrency: 4–10 per run at 10, and 96–100 at 100 (one run had
16), where at most 100 requests are in flight.
The service never returned `Unavailable`, so `REDIS_TIMEOUT=50ms` was never hit
under this load.

### Deny path (C)

C50 against B50 (same concurrency, one key): RPS +4.1 %, p50 −5.2 %
(*derived*), and Redis CPU 75.01 % against 83.64 %. This fits a denied request
writing nothing. With 100 tokens per minute and a fresh key, almost every
request in the 30 s run is a denial. The allowed/denied split was not
recorded, though.

## What this does not measure

- **A network between the parts.** ghz, the service and Redis run on one
  machine. No cloud network, and Redis is never on its own host.
- **Hosted Redis.** Upstash or any managed Redis. `scripts/load.sh` is meant for
  a local Redis only.
- **More than one Redis node.** No replicas, no Redis Cluster. The remark in
  finding 2 about a hot key on a cluster is a prediction, not a measurement.
- **Native Linux.** Docker ran inside the WSL2 VM (see Setup).
- **The sliding window algorithm.** `deploy/policies.load.yaml` has token
  buckets only.
- **More than one limiter instance** behind a load balancer.
- **Long runs.** Each measured run is 30 s. There is no soak test.

## Gaps

- No concurrency-1 run, so there is no queue-free latency for the service.
- Redis CPU is one sample per run, not an average over the run.
- go-redis pool stats were not recorded, so pool wait is unconfirmed (finding 3).
- ghz's own CPU was not recorded, and ghz used a single connection.
- The profile is one run, taken with pprof on; the matrix was not repeated with it.
- C50's allowed/denied split was not recorded.
- The Redis call alone was not timed; the timeouts use the whole-call p99 as an upper bound.
- The two floors are measured differently: `redis-cli` PING versus a Go
  `EVALSHA` benchmark from an earlier commit.

## Timeouts

Both timeouts were checked against these numbers, and neither was changed.

| Setting | Value | Why |
| --- | --- | --- |
| `REDIS_TIMEOUT` | 50 ms | The p99 of a whole `Allow` call at A100 was 9.76 ms (median of 3). A Redis call is only part of that, so 9.76 ms is an upper bound. 2 to 3 times it is 19.5 to 29.3 ms (*derived*), and 50 ms is above that. No request hit it: the service log has no `Unavailable`. |
| SDK `Timeout` | 100 ms | It has to be above `REDIS_TIMEOUT` plus the network. When Redis is down the limiter answers after about 51 ms (README, chaos test), so 100 ms leaves about 49 ms. |
| go-redis `PoolSize` | default | The pool stats were not recorded, so the data says nothing about it. It stays at the default. |

The p99 of the Redis call alone was not measured (see Gaps): `ratelimiter_redis_duration_seconds` was not scraped during the runs. The 50 ms value rests on the upper bound above.

## Reproduce

```bash
make up      # Redis 8 in Docker
make load    # builds and starts the service, runs the whole matrix
```

- Needs `docker compose`, `ghz`, `go` and `python3`. Ports 8080 and 9090 must
  be free. `make load` starts and stops the service itself.
- 21 runs of 10 s warm-up plus 30 s measured: about 15 minutes.
- Output goes to `docs/benchmarks/raw/`: `<run>.json` (ghz summary, latency
  distribution and histogram; the per-request records are stripped to keep the
  files small), `<run>.cpu.txt`, `meta.txt` and `service.log`.
- Settings can be changed with `DURATION`, `WARMUP`, `REPEATS`, `CONNECTIONS`
  and `OUT_DIR`.

To get the medians in the Results table from the raw files (run from the repo root):

```python
import glob, json, re, statistics
runs = {}
for path in sorted(glob.glob("docs/benchmarks/raw/*.json")):
    name = re.match(r"(.+)-r\d+\.json", path.split("/")[-1]).group(1)
    d = json.load(open(path))
    lat = {x["percentage"]: x["latency"] / 1e6 for x in d["latencyDistribution"]}
    cpu = dict(l.split() for l in open(path[:-5] + ".cpu.txt"))
    runs.setdefault(name, []).append({
        "rps": d["rps"], "p50": lat[50], "p95": lat[95], "p99": lat[99],
        "non-OK": sum(v for k, v in d["statusCodeDistribution"].items() if k != "OK"),
        "service CPU %": float(cpu["service_cpu_percent_of_one_core"]),
        "Redis CPU %": float(cpu["redis_cpu_percent_docker_stats"].rstrip("%")),
    })
for name in sorted(runs, key=lambda n: (n[0], int(n[1:]))):
    med = {k: statistics.median(r[k] for r in runs[name]) for k in runs[name][0]}
    print(f"{name}: " + "  ".join(f"{k} {v:.2f}" for k, v in med.items()))
```

Run it with `python3 -I`. RPS is the median of the three runs' RPS, and so on
for each column.
