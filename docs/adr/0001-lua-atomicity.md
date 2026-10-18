# 0001. Every decision is one Lua script

Status: Accepted | Date: 2026-10-15

## Context

A rate limiting decision is read state, decide, write state. Several service
instances share one Redis, so many callers run this sequence on the same key at
the same time. If two of them read the same state before either writes, both
get a yes and the limit is exceeded.

## Decision

Each decision (Allow and Peek, for both algorithms) is a single Lua script run
inside Redis. Redis executes a script as one uninterrupted step, so no other
command can run between the read and the write.

Keys are built in Go and passed in `KEYS`, never built inside Lua, so every key
a script touches is declared up front (see the hash tags in
[`keys.go`](../../internal/limiter/keys.go)).

## Consequences

- Counts stay exact under concurrency.
  [`accuracy_test.go`](../../test/load/accuracy_test.go) sends 2000
  attempts from 200 goroutines, with one and with two Redis clients, against a
  limit of 500; exactly 500 are allowed, on both algorithms.
- One round trip per decision.
- A denied request writes nothing, so a flood of denials adds little load.
- Peek repeats the math of Allow in a second, read-only script. Two copies can
  drift, so [`peek_test.go`](../../internal/limiter/peek_test.go) checks that
  Peek equals the Allow that follows it.
- Scripts for one key run one after another on Redis's single thread, so a very
  hot key is limited by Redis, not by the service. Measured in
  [benchmarks](../benchmarks.md): one hot key was no slower than 10,000 keys.
- Logic lives in Lua, which is harder to debug than Go. Each script was run by
  hand with `redis-cli --eval` before any Go code wrapped it.

## Alternatives considered

- **`INCR` + `EXPIRE`.** Two commands, so not atomic: a crash or disconnect
  between them leaves a counter with no TTL that never resets. It also cannot
  express a token bucket's read-then-decide step.
- **`WATCH` / `MULTI`.** Optimistic locking: under contention the transaction
  aborts and the client retries, each retry costing a round trip. The hottest
  keys, the ones that most need limiting, would retry the most.
- **A client-side or distributed lock.** More round trips, and the lock needs
  its own expiry and crash handling, which is the same problem one level up.

## Code

- The scripts: [`internal/limiter/scripts/`](../../internal/limiter/scripts/),
  embedded and run with `EVALSHA` in [`scripts.go`](../../internal/limiter/scripts.go).
- Tests: [`accuracy_test.go`](../../test/load/accuracy_test.go),
  [`peek_test.go`](../../internal/limiter/peek_test.go),
  [`e2e_test.go`](../../test/e2e/e2e_test.go) (one limit across two servers).
