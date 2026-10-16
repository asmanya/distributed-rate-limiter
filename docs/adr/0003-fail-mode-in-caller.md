# 0003. The failure mode lives in the caller

Status: Accepted | Date: 2026-10-16

## Context

Sometimes the limiter cannot give an answer: Redis is down or slow, the service
is restarting or deploying, the network between caller and service is broken.
In every one of these cases the caller holds a request and no decision, and it
still has to do something with it: let it through (fail open) or reject it (fail
closed).

The right choice depends on what is being protected, and only the caller knows
that. Letting a search request through during an outage is harmless; letting
unlimited login attempts through is not. The service cannot make this choice
well, and when the service itself is the part that is unreachable it cannot make
it at all.

## Decision

The service only says that it could not decide:

- A Redis error or timeout becomes `Unavailable`, returned within
  `REDIS_TIMEOUT`, with a generic message (details stay in the logs).
- The caller's own deadline or cancellation stays `DeadlineExceeded` or
  `Canceled`. A caller that gave up on its own is not reported as a store
  outage, so it does not trigger the failure mode by mistake.
- A denial is not an error. It is an `OK` response with `allowed=false`.
- Health stays `SERVING` while Redis is down, so the platform does not restart a
  service that is working and waiting for its store.

Whether to fail open or closed is decided by the caller, per call site, through
the configuration of the client SDK (`pkg/client`, not built yet). The service
has no failure-mode setting.

## Alternatives considered

**A failure mode per policy, configured in the service.** Rejected. It does not
work in the case that matters: if the limiter is unreachable, the caller never
reads the service's configuration. The caller also needs error handling for a
dead network anyway, so this would keep the real decision in two places that can
drift apart.

**One fixed behaviour in the service**, for example answering `allowed=true`
whenever Redis fails. Rejected. It makes "allowed" and "could not check" look the
same to the caller, hides the outage from metrics and logs on the caller's side,
and silently removes the limit exactly where it is needed most.

## Consequences

- The service stays small and honest: it either returns a decision or says it
  cannot. The behaviour is tested in `internal/service` and shown end to end in
  [chaos.md](../chaos.md) (Redis paused and stopped: `Unavailable` at about the
  50 ms timeout, health still `SERVING`).
- Every caller has to choose. A wrong choice has a cost either way: failing open
  during an outage lets traffic past the limit, failing closed turns a Redis
  outage into an outage of the caller.
- The SDK is not built yet. Until it exists, a caller using gRPC directly must
  handle `Unavailable` itself. The SDK should make the choice explicit rather
  than hide it behind a default.

## References

- Envoy's rate limit filter takes the same position: `failure_mode_deny` is set
  on the caller side, and when it is true "Envoy will not allow traffic in case
  of communication failure".
  [Envoy rate limit filter](https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/http/ratelimit/v3/rate_limit.proto)
- Stripe's guidance is to fail open: "if there were bugs in the rate limiting
  code (or if Redis were to go down), requests wouldn't be affected".
  [Scaling your API with rate limiters](https://stripe.com/blog/rate-limiters)
  (Paul Tarjan, 2017)
