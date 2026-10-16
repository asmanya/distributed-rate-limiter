package client

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

func ctxWithMetadata(kv ...string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(kv...))
}

func ctxWithPeer(addr string) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: remoteAddr(addr)})
}

func httpCtx(remote string, headers ...string) context.Context {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Add(headers[i], headers[i+1])
	}
	return withHTTPRequest(context.Background(), r)
}

func TestKeyFromMetadata(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		ask  string
		want string
	}{
		{"value present", ctxWithMetadata("x-api-key", "abc"), "x-api-key", "abc"},
		{"name in another case", ctxWithMetadata("x-api-key", "abc"), "X-Api-Key", "abc"},
		{"several values: the first", ctxWithMetadata("x-api-key", "first", "x-api-key", "second"), "x-api-key", "first"},
		{"whitespace is trimmed", ctxWithMetadata("x-api-key", "  abc  "), "x-api-key", "abc"},
		{"only whitespace", ctxWithMetadata("x-api-key", "   "), "x-api-key", ""},
		{"other entry only", ctxWithMetadata("x-other", "abc"), "x-api-key", ""},
		{"no metadata at all", context.Background(), "x-api-key", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KeyFromMetadata(tt.ask)(tt.ctx); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestKeyFromHeader(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		ask  string
		want string
	}{
		{"header present", httpCtx("192.0.2.1:1", "X-Api-Key", "abc"), "X-Api-Key", "abc"},
		{"name in another case", httpCtx("192.0.2.1:1", "X-Api-Key", "abc"), "x-api-key", "abc"},
		{"whitespace is trimmed", httpCtx("192.0.2.1:1", "X-Api-Key", " abc "), "X-Api-Key", "abc"},
		{"header missing", httpCtx("192.0.2.1:1"), "X-Api-Key", ""},
		{"a gRPC context has no HTTP headers", context.Background(), "X-Api-Key", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KeyFromHeader(tt.ask)(tt.ctx); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestKeyFromPeer(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"ipv4 without the port", ctxWithPeer("192.0.2.1:5555"), "192.0.2.1"},
		{"ipv6 without the port", ctxWithPeer("[2001:db8::1]:443"), "2001:db8::1"},
		{"no port to strip", ctxWithPeer("bufconn"), "bufconn"},
		{"no peer", context.Background(), ""},
		{"peer without an address", peer.NewContext(context.Background(), &peer.Peer{}), ""},
		{"a real net.TCPAddr", peer.NewContext(context.Background(),
			&peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 80}}), "203.0.113.9"},
		{"http remote address", httpCtx("203.0.113.7:9999"), "203.0.113.7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KeyFromPeer(tt.ctx); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// The port changes with every connection: it must not be part of the key.
func TestKeyFromPeerIgnoresThePort(t *testing.T) {
	a := KeyFromPeer(ctxWithPeer("192.0.2.1:1111"))
	b := KeyFromPeer(ctxWithPeer("192.0.2.1:2222"))
	if a != b {
		t.Errorf("same IP, different ports gave %q and %q", a, b)
	}
}

func TestResolveKeyFallback(t *testing.T) {
	fromHeader := KeyFromMetadata("x-api-key")
	tests := []struct {
		name string
		ctx  context.Context
		fn   KeyFunc
		want string
	}{
		{"the key from fn", peer.NewContext(ctxWithMetadata("x-api-key", "abc"), &peer.Peer{Addr: remoteAddr("192.0.2.1:1")}), fromHeader, "abc"},
		{"fn gives nothing: the peer IP", ctxWithPeer("192.0.2.1:1"), fromHeader, "192.0.2.1"},
		{"no fn: the peer IP", ctxWithPeer("192.0.2.1:1"), nil, "192.0.2.1"},
		{"no key and no peer: one shared bucket", context.Background(), fromHeader, anonymousKey},
		{"no fn and no peer: one shared bucket", context.Background(), nil, anonymousKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveKey(tt.ctx, tt.fn); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSafeKey(t *testing.T) {
	long := strings.Repeat("a", maxKeyLen)
	tests := []struct {
		name     string
		in       string
		wantSame bool
	}{
		{"a normal key", "user-1", true},
		{"a key with a colon", "tenant:user-1", true},
		{"exactly the maximum length", long, true},
		{"one byte too long", long + "a", false},
		{"a brace", "a{b", false},
		{"both braces", "a{b}c", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := safeKey(tt.in)
			if tt.wantSame && got != tt.in {
				t.Errorf("got %q, want the key unchanged", got)
			}
			if !tt.wantSame && (!strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64) {
				t.Errorf("got %q, want a sha256 hash", got)
			}
		})
	}

	if got := safeKey(""); got != anonymousKey {
		t.Errorf("empty key: got %q, want %q", got, anonymousKey)
	}
	t.Run("the same input gives the same hash, different inputs differ", func(t *testing.T) {
		a, b := safeKey(long+"a"), safeKey(long+"b")
		if safeKey(long+"a") != a {
			t.Error("the hash is not stable")
		}
		if a == b {
			t.Error("different inputs gave one hash")
		}
	})
}

// The security check: whatever the request carries, the key the service sees is
// one it accepts. An InvalidArgument here would be treated as a limiter failure,
// and with FailOpen the caller would go unlimited.
func TestResolveKeyAlwaysAcceptable(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"only spaces", "   "},
		{"300 bytes", strings.Repeat("x", 300)},
		{"braces", "{evil}"},
		{"a normal key", "user-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := httpCtx("192.0.2.1:1", "X-Api-Key", tt.value)
			got := resolveKey(ctx, KeyFromHeader("X-Api-Key"))

			if got == "" || len(got) > maxKeyLen || strings.ContainsAny(got, "{}") {
				t.Errorf("key %q would be refused by the service", got)
			}
		})
	}
}
