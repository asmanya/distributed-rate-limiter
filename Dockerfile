# syntax=docker/dockerfile:1

# Version 1, enough for the demo stack. A later version makes this production
# grade (smaller base, pinned digests, size checks).
#
# One Dockerfile builds both programs; PKG says which one:
#   docker build --build-arg PKG=./cmd/ratelimiter .
#   docker build --build-arg PKG=./examples/greeter .

FROM golang:1.27-alpine AS build
WORKDIR /src

# The dependencies first: this layer is reused until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# After the source copy on purpose: changing PKG must not throw away the cache
# of the layers above.
ARG PKG=./cmd/ratelimiter
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ${PKG}

FROM alpine:3.22
COPY --from=build /out/app /app
USER nobody
# Exec form: SIGTERM reaches the program itself, with no shell in between, so
# the graceful shutdown works.
ENTRYPOINT ["/app"]
