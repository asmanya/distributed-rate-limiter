# Chaos test: Redis failures and shutdown

What the service does when Redis is slow, stopped or back again, and when the
process is told to stop under load. Every number below was printed by
[`scripts/chaos.sh`](../scripts/chaos.sh); nothing was written by hand. The
output is trimmed to the relevant lines.

| | |
| --- | --- |
| Run | 2026-10-16, commit `489e9a4` |
| Machine | laptop, WSL2, Redis 8 in Docker on the same host |
| Settings | defaults: `REDIS_TIMEOUT=50ms`, `SHUTDOWN_TIMEOUT=10s` |
| Policy | `search` (token bucket, 100 per minute), namespace `chaos` |

Reproduce:

```bash
bash scripts/chaos.sh
```

It needs `docker compose`, `redis-cli`, `grpcurl`, `ghz` and `curl`, and uses
ports 8080 and 9090. It prints `CHECK PASS` / `CHECK FAIL` lines and cleans up
after itself (service stopped, Redis started again).

## How to read the times

- **client-side** is measured around `grpcurl`, so it includes grpcurl's own
  start-up. In the baseline, with Redis healthy, it is about 14 ms.
- **service log `duration_ms`** is the time the service spent on the RPC. Only
  failed calls appear there: successful calls are logged at debug level.
- This is one run on one machine. It shows behaviour, not performance.

## Summary

| Stage | Expected | Result |
| --- | --- | --- |
| 1 Baseline | `OK` | 5 of 5 `OK` |
| 2 Redis pause (2 s) | `Unavailable` within the timeout | `Unavailable`, 50.768 ms server-side |
| 3 Redis stop | `Unavailable`, process alive, health `SERVING` | 3 of 3 `Unavailable` at 50.7 to 51.1 ms, process alive, health `SERVING` |
| 4 Redis start | `OK` again without a restart, errors counter flat | `OK` on the second attempt, 606 ms after the start command began; errors counter 5 before and after |
| 5 SIGTERM under load | clean exit | exit code 0, shutdown took 0.491 ms, no forced stop |

## 1. Baseline

Light load, Redis healthy.

```text
Allow -> OK (client-side 14 ms)        (five times)
grpc_server_handled_total{code="OK",method="/ratelimiter.v1.RateLimitService/Allow"} 5
ratelimiter_redis_duration_seconds_count{script="tb_allow"} 5
```

Five calls, five `OK`, five Redis round trips recorded, no errors.

## 2. Redis pause

```bash
redis-cli CLIENT PAUSE 2000
```

Redis accepts connections but answers nothing for two seconds.

```text
Allow -> Unavailable (client-side 68 ms)
  service log: severity=WARN code=Unavailable duration_ms=50.768
grpc_server_handled_total{code="Unavailable",...} 1
ratelimiter_redis_errors_total{script="tb_allow"} 1
Allow -> OK (client-side 16 ms)          <- after the pause ended
```

The call fails with `Unavailable` after the 50 ms budget, not after the two
seconds Redis was paused. The errors counter went up by one, and the next call
after the pause succeeded without any restart.

## 3. Redis stop

```bash
docker compose stop redis
```

```text
Allow -> Unavailable (client-side 65 ms)
Allow -> Unavailable (client-side 63 ms)
Allow -> Unavailable (client-side 65 ms)
  service log: severity=WARN code=Unavailable duration_ms=50.742
  service log: severity=WARN code=Unavailable duration_ms=51.051
  service log: severity=WARN code=Unavailable duration_ms=51.081
$ grpc.health.v1.Health/Check
{ "status": "SERVING" }
ratelimiter_redis_errors_total{script="tb_allow"} 4
```

The service log for each failure, at `ERROR` level:

```json
{"severity":"ERROR","message":"rate limit store failed","error":"store unavailable: redis call exceeded 50ms"}
```

All three calls failed at about the timeout. The process stayed alive and the
health check still said `SERVING`: a Redis outage does not make the platform
restart the service. The client is given the generic message
`rate limit store unavailable`, never the Redis host or port (covered by
`TestStoreErrorIsNotLeaked`).

Worth noting: a stopped Redis took as long to fail as a paused one (about
50 ms), not less. We did not investigate why.

## 4. Redis start

```bash
docker compose start redis
```

```text
Allow -> Unavailable (client-side 14 ms)
Allow -> OK (client-side 14 ms)
first OK came 606 ms after the start command began (includes docker's own start time)
ratelimiter_redis_errors_total{script="tb_allow"}: before=5 after=5     (five more calls in between)
```

The service was not restarted. The first call after the start command still
failed, the second succeeded. The 606 ms includes Docker starting the container
and the script's 250 ms polling interval, so it is an upper bound on how fast
the service reconnects, not a measurement of it. With Redis healthy again the
errors counter stayed at 5 across five further calls.

## 5. SIGTERM under load

`ghz` sends 100 requests per second from 5 workers for 6 seconds; two seconds in,
the script sends `SIGTERM` to the service.

```text
service exit code: 0
{"severity":"INFO","message":"shutting down","timeout":10000000000}
{"severity":"INFO","message":"shutdown complete","duration_ms":0.491}

Status code distribution:
  [OK]            191 responses
  [Unavailable]   408 responses

Error distribution:
  [408]   rpc error: code = Unavailable desc = connection error: desc = "transport: Error while dialing: dial tcp 127.0.0.1:8080: connect: connection refused"
```

The process exited with code 0 and did not need the forced stop. All 408
failures are one error, `connection refused`: they are requests `ghz` kept
sending after the service had stopped listening. No request failed while it was
being served (no `transport is closing`, no `EOF`).

At this rate there was little in flight at the moment of `SIGTERM` (shutdown
took 0.491 ms), so this run is weak evidence for in-flight requests. That
guarantee is tested directly in `internal/server/server_test.go`
(`TestShutdownLetsInFlightFinish`).

## What this does not show

- Performance under load: that needs a separate load test.
- A Redis that is slow but not stopped, only paused and stopped.
- Cloud Run: it gives a container 10 seconds between `SIGTERM` and `SIGKILL`,
  so a deployment there should set `SHUTDOWN_TIMEOUT` below that (the default is
  10 s).
