.PHONY: tools proto lint test test-integration up down

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
