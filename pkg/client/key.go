package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// anonymousKey is the shared bucket for a request that carries no key at all.
// Skipping the limit instead would let anyone who leaves the key out go unlimited.
const anonymousKey = "anonymous"

// maxKeyLen is the longest key the limiter service accepts.
const maxKeyLen = 256

// KeyFunc says what is being limited. It returns the key for a request, or ""
// when the request does not carry one; the interceptor and the middleware then
// fall back to the caller's IP address, and then to one shared bucket.
//
// The request arrives as a context, so one KeyFunc type serves both gRPC and HTTP.
type KeyFunc func(ctx context.Context) string

// KeyFromMetadata returns a KeyFunc that reads the gRPC metadata entry name, for
// example an API key or a user id. Names are matched without regard to case.
func KeyFromMetadata(name string) KeyFunc {
	return func(ctx context.Context) string {
		values := metadata.ValueFromIncomingContext(ctx, name)
		if len(values) == 0 {
			return ""
		}
		return strings.TrimSpace(values[0])
	}
}

// KeyFromHeader returns a KeyFunc that reads the HTTP header name.
//
// A client can send any header it likes. Use one that identifies the caller only
// if your own proxy sets it and overwrites whatever the client sent, otherwise
// the limit can be dodged by changing the value on every request.
func KeyFromHeader(name string) KeyFunc {
	return func(ctx context.Context) string {
		h, ok := ctx.Value(httpHeaderKey{}).(http.Header)
		if !ok {
			return ""
		}
		return strings.TrimSpace(h.Get(name))
	}
}

// KeyFromPeer is a KeyFunc that uses the caller's IP address. The port is left
// out: it changes with every connection and would give each one its own limit.
//
// Behind a proxy or a load balancer (Cloud Run, Envoy) the peer is the proxy, so
// every user would share one key. There, use KeyFromHeader with a header your
// proxy sets.
func KeyFromPeer(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return ""
	}
	addr := p.Addr.String()
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr // no port to strip, e.g. a unix socket
}

// resolveKey is what the interceptor and the middleware call. It never returns
// an empty or unusable key: the key from fn, else the peer IP, else one shared
// bucket, and always in a form the service accepts.
func resolveKey(ctx context.Context, fn KeyFunc) string {
	if fn == nil {
		fn = KeyFromPeer
	}
	k := fn(ctx)
	if k == "" {
		k = KeyFromPeer(ctx)
	}
	if k == "" {
		k = anonymousKey
	}
	return safeKey(k)
}

// safeKey returns k if the service accepts it, and a hash of it if not.
//
// This matters for security. The service answers InvalidArgument to an empty key,
// one over 256 bytes or one with '{' or '}', and the SDK treats that like any
// failure: with FailOpen the request goes through. A caller who sends such a key
// would be unlimited. A hash keeps them in one bucket of their own.
//
// The rules are the service's (internal/limiter, which this package cannot
// import). Keep them in step.
func safeKey(k string) string {
	if k == "" {
		return anonymousKey
	}
	if len(k) <= maxKeyLen && !strings.ContainsAny(k, "{}") {
		return k
	}
	sum := sha256.Sum256([]byte(k))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// httpHeaderKey is the context key under which the middleware leaves the
// request's headers, so a KeyFunc can read them.
type httpHeaderKey struct{}

// withHTTPRequest puts what a KeyFunc needs from an HTTP request into ctx: the
// headers, and the remote address as the peer, so KeyFromPeer works the same way
// it does for gRPC.
func withHTTPRequest(ctx context.Context, r *http.Request) context.Context {
	ctx = context.WithValue(ctx, httpHeaderKey{}, r.Header)
	if r.RemoteAddr != "" {
		ctx = peer.NewContext(ctx, &peer.Peer{Addr: remoteAddr(r.RemoteAddr)})
	}
	return ctx
}

// remoteAddr is an http.Request's RemoteAddr as a net.Addr.
type remoteAddr string

func (a remoteAddr) Network() string { return "tcp" }
func (a remoteAddr) String() string  { return string(a) }
