# syntax=docker/dockerfile:1

# One Dockerfile builds both programs; PKG says which one:
#   docker build --build-arg PKG=./cmd/ratelimiter .
#   docker build --build-arg PKG=./examples/greeter .
#
# The last stage is the production image, so a plain `docker build` and Cloud
# Run's --source build both get it. docker-compose.yml builds the `demo` stage.

FROM golang:1.27 AS build
WORKDIR /src

# The dependencies first: this layer is reused until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# After the source copy on purpose: changing PKG or VERSION must not throw away
# the cache of the layers above.
ARG PKG=./cmd/ratelimiter
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/app ${PKG}

# Demo stage: alpine only because the compose healthcheck needs `nc`, and
# distroless has no shell or tools. Never deployed.
FROM alpine:3.22 AS demo
COPY --from=build /out/app /app
USER nobody
ENTRYPOINT ["/app"]

# Production stage: no shell, no package manager, runs as non-root.
# Debugging with `docker exec` is not possible here; use the :debug-nonroot
# variant of this base locally if you need a shell.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/app /app
# POLICY_FILE defaults to the relative path deploy/policies.yaml, which resolves
# against WORKDIR /. Unused by the greeter.
COPY deploy/policies.yaml /deploy/policies.yaml
USER nonroot:nonroot
# Exec form: SIGTERM reaches the program itself, with no shell in between, so
# the graceful shutdown works.
ENTRYPOINT ["/app"]
