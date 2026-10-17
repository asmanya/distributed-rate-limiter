#!/usr/bin/env bash
# Load test for the rate limiter: the run matrix from the Phase 5 plan.
#
#   A  10k random keys, policy load_open (never denies)   normal multi-user traffic
#   B  one fixed key,   policy load_open (never denies)   single-key contention
#   C  one fixed key,   policy load_hot  (100 per minute) the deny path
#
# Each scenario runs at concurrency 10, 50 and 100 (C only at 50), REPEATS times.
# Every run is a warm-up (result thrown away) followed by a measured run.
# The raw ghz output of the measured run is the evidence; docs/benchmarks.md is
# built from it.
#
# Needs: docker compose (Redis, `make up`), ghz, go, python3. Uses ports 8080 and
# 9090. Local only: never point this at a hosted Redis.
#
# Knobs (environment): DURATION, WARMUP, REPEATS, CONNECTIONS, OUT_DIR.
set -euo pipefail
cd "$(dirname "$0")/.."

DURATION=${DURATION:-30s}
WARMUP=${WARMUP:-10s}
REPEATS=${REPEATS:-3}
CONNECTIONS=${CONNECTIONS:-1}
OUT_DIR=${OUT_DIR:-docs/benchmarks/raw}
GRPC=localhost:8080
CALL=ratelimiter.v1.RateLimitService/Allow
SVC_PID=""
BIN=""

mkdir -p "$OUT_DIR"

cleanup() {
  if [ -n "$SVC_PID" ]; then
    kill "$SVC_PID" 2>/dev/null || true
    wait "$SVC_PID" 2>/dev/null || true
  fi
  if [ -n "$BIN" ]; then rm -f "$BIN"; fi
}
trap cleanup EXIT

# secs turns "30s" into 30. ghz takes the duration with its unit, the CPU maths
# below needs plain seconds.
secs() { echo "${1%s}"; }

# cpu_ticks prints user+system CPU time of the service in clock ticks (the
# kernel counts 100 per second). Fields 14 and 15 of /proc/<pid>/stat.
cpu_ticks() { awk '{print $14 + $15}' "/proc/$SVC_PID/stat"; }

# payload <scenario> <tag> prints the request JSON. The tag keeps repeats apart:
# a hot key used by one run is never reused by the next.
payload() {
  case $1 in
    A) echo '{"namespace":"load","policy":"load_open","key":"k-{{randomInt 0 10000}}"}' ;;
    B) echo "{\"namespace\":\"load\",\"policy\":\"load_open\",\"key\":\"hot-$2\"}" ;;
    C) echo "{\"namespace\":\"load\",\"policy\":\"load_hot\",\"key\":\"deny-$2\"}" ;;
  esac
}

ghz_run() { # ghz_run <duration> <concurrency> <payload> [extra ghz args...]
  local dur=$1 conc=$2 data=$3
  shift 3
  ghz --insecure --call "$CALL" -d "$data" -z "$dur" -c "$conc" \
    --connections "$CONNECTIONS" "$@" "$GRPC"
}

# strip_details drops ghz's per-request records ("details", about 50 MB per run).
# The summary, the latency distribution and the histogram stay: they are what the
# numbers in docs/benchmarks.md come from.
strip_details() {
  python3 -I - "$1" <<'PY'
import json, sys
path = sys.argv[1]
with open(path) as f:
    d = json.load(f)
d.pop("details", None)
with open(path, "w") as f:
    json.dump(d, f, indent=1)
PY
}

run_one() { # run_one <scenario> <concurrency> <repeat>
  local sc=$1 conc=$2 rep=$3
  local name="${sc}${conc}-r${rep}"
  local tag="$name-$$" # $$ keeps the key unique across invocations of this script
  local json="$OUT_DIR/$name.json" cpu="$OUT_DIR/$name.cpu.txt"

  printf '\n=== %s (scenario %s, concurrency %s, repeat %s) ===\n' "$name" "$sc" "$conc" "$rep"

  # Warm-up has its own key, so scenario C does not spend its 100 tokens here.
  ghz_run "$WARMUP" "$conc" "$(payload "$sc" "warm-$tag")" >/dev/null 2>&1

  local t0 t1
  t0=$(cpu_ticks)
  # Redis CPU is sampled once, in the middle of the measured run.
  (
    sleep $(($(secs "$DURATION") / 2))
    docker stats --no-stream --format '{{.CPUPerc}}' "$(docker compose ps -q redis)" >"$cpu.redis"
  ) &
  local sampler=$!
  ghz_run "$DURATION" "$conc" "$(payload "$sc" "$tag")" -O json -o "$json"
  wait "$sampler"
  t1=$(cpu_ticks)
  strip_details "$json"

  # Service CPU as a percentage of one core, over the measured run.
  awk -v a="$t0" -v b="$t1" -v d="$(secs "$DURATION")" \
    'BEGIN { printf "service_cpu_percent_of_one_core %.1f\n", (b - a) / 100 / d * 100 }' >"$cpu"
  printf 'redis_cpu_percent_docker_stats %s\n' "$(tr -d '\n' <"$cpu.redis")" >>"$cpu"
  rm -f "$cpu.redis"

  python3 -I - "$json" "$cpu" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
lat = {x["percentage"]: x["latency"] / 1e6 for x in d.get("latencyDistribution") or []}
print("count %d  rps %.0f  p50 %.2f ms  p95 %.2f ms  p99 %.2f ms" % (
    d["count"], d["rps"], lat.get(50, 0), lat.get(95, 0), lat.get(99, 0)))
print("status:", d["statusCodeDistribution"])
print(open(sys.argv[2]).read().strip())
PY
}

# --- start ---------------------------------------------------------------
docker compose ps --status running redis | grep -q redis || {
  echo "Redis is not running: make up" >&2
  exit 1
}

BIN=$(mktemp)
go build -o "$BIN" ./cmd/ratelimiter

# Config recorded next to the results, so the numbers say what they are numbers of.
{
  echo "commit: $(git rev-parse --short HEAD)"
  echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "duration: $DURATION  warmup: $WARMUP  repeats: $REPEATS  connections: $CONNECTIONS"
  echo "service: POLICY_FILE=deploy/policies.load.yaml LOG_LEVEL=warn, other settings default"
} >"$OUT_DIR/meta.txt"
cat "$OUT_DIR/meta.txt"

POLICY_FILE=deploy/policies.load.yaml LOG_LEVEL=warn REDIS_URL=redis://localhost:6379/0 \
  "$BIN" >"$OUT_DIR/service.log" 2>&1 &
SVC_PID=$!
for _ in $(seq 1 40); do
  if ghz --insecure --call grpc.health.v1.Health/Check -n 1 "$GRPC" >/dev/null 2>&1; then break; fi
  sleep 0.25
done

# --- matrix --------------------------------------------------------------
for rep in $(seq 1 "$REPEATS"); do
  for sc in A B; do
    for conc in 10 50 100; do
      run_one "$sc" "$conc" "$rep"
    done
  done
  run_one C 50 "$rep"
done

echo
echo "done: raw results in $OUT_DIR"
