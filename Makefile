.PHONY: tools proto lint test test-integration up down run load

# Install the proto plugins pinned in go.mod (tool directive).
tools:
	go install tool

# Lint the contract, then regenerate gen/ from it.
proto:
	buf lint
	buf generate

lint:
	golangci-lint run
	buf lint

# Unit tests only; integration tests are behind a build tag.
test:
	go test -race ./...

# Needs a real Redis (make up); the Redis address comes from the environment.
test-integration:
	go test -race -tags=integration ./...

# Start local Redis and wait until its healthcheck passes.
up:
	docker compose up -d --wait

down:
	docker compose down

# Run the service, loading .env first if it exists. One line on purpose: every
# recipe line is its own shell, so `set -a` (export everything sourced) and the
# `go run` must share one.
run:
	@set -a; if [ -f .env ]; then . ./.env; fi; set +a; go run ./cmd/ratelimiter

# The load test matrix (about 15 minutes). Needs Redis (make up) and ghz; results
# go to docs/benchmarks/raw. Local only, never against a hosted Redis.
load:
	bash scripts/load.sh
