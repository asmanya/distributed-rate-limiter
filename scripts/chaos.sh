#!/usr/bin/env bash
# Chaos test for the rate limiter: what happens when Redis misbehaves and when the
# process is told to stop. Stages, in order:
#   1 baseline        light load, everything works
#   2 redis pause     Redis stops answering for 2s
#   3 redis stop      Redis is gone
#   4 redis start     Redis is back, no restart of the service
#   5 sigterm         the service is stopped under load
#
# (Pause comes before stop: CLIENT PAUSE needs a running Redis.)
#
# Needs: docker compose, redis-cli, grpcurl, ghz, curl. Uses ports 8080 and 9090.
# Everything the script prints is also written to $OUT/chaos.log; docs/chaos.md is
# built from that file.
set -euo pipefail
cd "$(dirname "$0")/.."

OUT=${OUT:-$(mktemp -d)}
GRPC=localhost:8080
METRICS=localhost:9090
REQ='{"namespace":"chaos","policy":"search","key":"k"}'
SVC_PID=""
FAILED=0
LAST_CODE=""

mkdir -p "$OUT"
exec > >(tee "$OUT/chaos.log") 2>&1

# Whatever happens, leave the machine as we found it: service stopped, Redis up.
cleanup() {
  if [ -n "$SVC_PID" ]; then
    kill "$SVC_PID" 2>/dev/null || true
  fi
  docker compose start redis >/dev/null 2>&1 || true
}
trap cleanup EXIT

stage() { printf '\n=== %s ===\n' "$*"; }
run() { printf '$ %s\n' "$*"; }

check() { # check <description> <command...>
  local what=$1
  shift
  if "$@"; then
    printf 'CHECK PASS: %s\n' "$what"
  else
    printf 'CHECK FAIL: %s\n' "$what"
    FAILED=1
  fi
}

# allow sends one Allow, prints the result and how long the client waited, and
# sets LAST_CODE to OK or the gRPC code name. The client-side time includes
# grpcurl's own start-up; the service-side time is in the "rpc" log lines.
allow() {
  local start end out
  start=$(date +%s%N)
  out=$(grpcurl -plaintext -d "$REQ" "$GRPC" ratelimiter.v1.RateLimitService/Allow 2>&1) || true
  end=$(date +%s%N)
  if grep -q '"allowed"' <<<"$out"; then
    LAST_CODE=OK
  else
    LAST_CODE=$(sed -nE 's/^ *Code: *([A-Za-z]+).*/\1/p' <<<"$out" | head -n1)
    LAST_CODE=${LAST_CODE:-UNKNOWN}
  fi
  printf 'Allow -> %s (client-side %d ms)\n' "$LAST_CODE" $(((end - start) / 1000000))
}

health() { grpcurl -plaintext "$GRPC" grpc.health.v1.Health/Check 2>&1 || true; }

snapshot() {
  curl -s "$METRICS/metrics" |
    grep -E '^(grpc_server_handled_total|ratelimiter_redis_errors_total|ratelimiter_redis_duration_seconds_count)' || true
}

errors_total() {
  curl -s "$METRICS/metrics" |
    awk '/^ratelimiter_redis_errors_total\{script="tb_allow"\}/ {print $2; found=1} END {if (!found) print 0}'
}

# rpc_lines shows the last n server-side "rpc" log lines: code and duration_ms.
rpc_lines() {
  grep '"message":"rpc"' "$OUT/service.log" | tail -n "$1" |
    sed -E 's/.*"severity":"([A-Z]+)".*"code":"([A-Za-z]+)","duration_ms":([0-9.]+).*/  service log: severity=\1 code=\2 duration_ms=\3/' || true
}

code_is() { [ "$LAST_CODE" = "$1" ]; }

# ---------------------------------------------------------------- setup
stage "setup"
if pgrep -x ratelimiter >/dev/null; then
  echo "a ratelimiter process is already running; stop it first" >&2
  exit 1
fi
run "make up"
make up
run "go build -o \$OUT/ratelimiter ./cmd/ratelimiter"
go build -o "$OUT/ratelimiter" ./cmd/ratelimiter
echo "commit: $(git rev-parse --short HEAD)   date: $(date -u +%Y-%m-%dT%H:%M:%SZ)   OUT=$OUT"

set -a
[ -f .env ] && . ./.env
set +a
"$OUT/ratelimiter" >"$OUT/service.log" 2>&1 &
SVC_PID=$!
echo "service started, pid $SVC_PID"
for _ in $(seq 1 40); do
  if grpcurl -plaintext "$GRPC" grpc.health.v1.Health/Check >/dev/null 2>&1; then break; fi
  sleep 0.25
done
health

# ---------------------------------------------------------------- 1 baseline
stage "1 baseline: light load, Redis healthy"
all_ok=1
for _ in 1 2 3 4 5; do
  allow
  code_is OK || all_ok=0
done
snapshot
check "baseline: 5 of 5 calls OK" [ "$all_ok" = 1 ]

# ---------------------------------------------------------------- 2 redis pause
stage "2 redis pause: Redis stops answering for 2s"
run "redis-cli CLIENT PAUSE 2000"
redis-cli CLIENT PAUSE 2000
allow
check "pause: Allow returns Unavailable" code_is Unavailable
rpc_lines 1
snapshot
sleep 2.5
allow
check "pause: OK again after the pause ended" code_is OK

# ---------------------------------------------------------------- 3 redis stop
stage "3 redis stop: Redis is gone"
run "docker compose stop redis"
docker compose stop redis
unavailable=0
for _ in 1 2 3; do
  allow
  code_is Unavailable && unavailable=$((unavailable + 1))
done
check "stop: 3 of 3 calls Unavailable" [ "$unavailable" = 3 ]
rpc_lines 3
run "grpc.health.v1.Health/Check"
h=$(health)
echo "$h"
check "stop: health is still SERVING" grep -q SERVING <<<"$h"
check "stop: service process is still alive" kill -0 "$SVC_PID"
snapshot
echo "service log (ERROR lines, last 2):"
grep '"severity":"ERROR"' "$OUT/service.log" | tail -n 2 || true

# ---------------------------------------------------------------- 4 redis start
stage "4 redis start: Redis is back, the service is not restarted"
t0=$(date +%s%N)
run "docker compose start redis"
docker compose start redis
recovered=0
for _ in $(seq 1 60); do
  allow
  if code_is OK; then
    recovered=1
    break
  fi
  sleep 0.25
done
echo "first OK came $(((($(date +%s%N)) - t0) / 1000000)) ms after the start command began (includes docker's own start time)"
check "start: Allow returns OK again without restarting the service" [ "$recovered" = 1 ]
before=$(errors_total)
for _ in 1 2 3 4 5; do allow; done
after=$(errors_total)
echo "ratelimiter_redis_errors_total{script=\"tb_allow\"}: before=$before after=$after"
check "start: errors counter is flat while Redis is healthy" [ "$before" = "$after" ]
snapshot

# ---------------------------------------------------------------- 5 sigterm
stage "5 sigterm under load"
run "ghz ... -c 5 --rps 100 -z 6s   (in the background)"
ghz --insecure --call ratelimiter.v1.RateLimitService.Allow -d "$REQ" -c 5 --rps 100 -z 6s "$GRPC" >"$OUT/ghz.txt" 2>&1 &
GHZ_PID=$!
sleep 2
run "kill -TERM $SVC_PID"
kill -TERM "$SVC_PID"
rc=0
wait "$SVC_PID" || rc=$?
SVC_PID=""
wait "$GHZ_PID" || true
echo "service exit code: $rc"
echo "service log:"
grep -E '"message":"(shutting down|shutdown complete|shutdown forced)"' "$OUT/service.log" || true
echo "ghz result:"
sed -n '/Status code distribution/,$p' "$OUT/ghz.txt"
check "sigterm: service exited with code 0" [ "$rc" = 0 ]
check "sigterm: 'shutdown complete' logged" grep -q '"message":"shutdown complete"' "$OUT/service.log"
check "sigterm: no 'shutdown forced'" bash -c "! grep -q '\"message\":\"shutdown forced\"' '$OUT/service.log'"

# ---------------------------------------------------------------- end
stage "result"
if [ "$FAILED" = 0 ]; then
  echo "ALL CHECKS PASSED"
else
  echo "SOME CHECKS FAILED"
fi
echo "raw output kept in $OUT"
exit "$FAILED"
