# 0002. The caller supplies the time

Status: Accepted | Date: 2026-10-15

## Context

Both algorithms need the current time: a token bucket to refill, a sliding
window to know how far into the window we are. Redis can provide its own clock
(`TIME`) or the caller can pass one in.

## Decision

The caller passes `now` in milliseconds as an argument. Scripts never call
`TIME`, and the limiter package never calls `time.Now()`. The service injects a
clock and passes the time down in `Request.Now`. The window index and start are
computed once in Go (`windowOf`) and passed to the script, so Go and Lua cannot
disagree about which window a request belongs to.

## Consequences

- Tests control time. A 10-second refill test runs in milliseconds with no
  `sleep`, and cannot be flaky.
- Scripts are deterministic: the same arguments and state give the same result.
  Some hosted Redis services are stricter about non-deterministic commands in
  scripts; whether the target one is is checked in Phase 6.
- Instances are assumed to have NTP-synced clocks. Their skew is milliseconds
  while windows are seconds long, so the effect on a decision is negligible.
- If an instance's clock is behind the stored timestamp, elapsed time would be
  negative. The scripts clamp it to zero, so the bucket never refills backwards
  (covered by the "now before stored timestamp" test in `tokenbucket_test.go`).
- A badly skewed instance, off by seconds or more, could refill early or late.
  v1 does not defend against that.

## Alternatives considered

- **Redis `TIME` inside the script.** One authoritative clock for all callers,
  but scripts become non-deterministic and tests cannot inject time, so every
  time-based test would need a real wait.
- **The caller reads Redis `TIME` first, then passes it in.** Same clock for
  everyone, but an extra round trip on every decision.
