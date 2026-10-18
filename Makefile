.PHONY: tools proto lint test test-integration up down run load deploy

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

# Deploy to Cloud Run, by hand (there is no CD). Needs gcloud logged in with the
# project set, the secret redis-url, and the service account (Phase 6).
#   --source          Cloud Build builds the last Dockerfile stage (distroless).
#   --use-http2       gRPC needs HTTP/2 all the way to the container.
#   --max-instances   caps cost and abuse; --min-instances 0 costs nothing idle.
#   ADMIN_ENABLED     Reset must never be reachable on a public endpoint.
#   SHUTDOWN_TIMEOUT  Cloud Run gives 10 s between SIGTERM and SIGKILL.
#   REDIS_TIMEOUT     a starting value, not measured from Cloud Run yet.
#   --set-secrets     the Redis URL comes from Secret Manager, never an env var.
GCP_REGION ?= asia-south1
GCP_SERVICE_ACCOUNT ?= ratelimiter-run@$(shell gcloud config get-value project 2>/dev/null).iam.gserviceaccount.com
deploy:
	gcloud run deploy ratelimiter \
		--source . \
		--region $(GCP_REGION) \
		--use-http2 \
		--allow-unauthenticated \
		--max-instances 2 \
		--min-instances 0 \
		--service-account $(GCP_SERVICE_ACCOUNT) \
		--set-env-vars ADMIN_ENABLED=false,LOG_LEVEL=info,SHUTDOWN_TIMEOUT=8s,REDIS_TIMEOUT=200ms \
		--set-secrets REDIS_URL=redis-url:latest
