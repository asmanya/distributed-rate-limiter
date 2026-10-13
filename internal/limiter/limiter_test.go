package limiter

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func validRequest() Request {
	return Request{
		Namespace: "orders-api",
		Policy:    "search",
		Key:       "user-1",
		Limit:     10,
		Period:    time.Minute,
		Cost:      1,
		Now:       time.UnixMilli(1_700_000_000_000),
	}
}

func TestRequestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Request)
		want   error
	}{
		{"empty key", func(r *Request) { r.Key = "" }, ErrInvalidKey},
		{"key 300 bytes", func(r *Request) { r.Key = strings.Repeat("a", 300) }, ErrInvalidKey},
		{"key at the 256 byte limit", func(r *Request) { r.Key = strings.Repeat("a", 256) }, nil},
		{"key with braces", func(r *Request) { r.Key = "user{1}" }, ErrInvalidKey},
		{"namespace with colon", func(r *Request) { r.Namespace = "orders:api" }, ErrInvalidName},
		{"period below 1ms", func(r *Request) { r.Period = 500 * time.Microsecond }, ErrInvalidPolicy},
		{"cost zero", func(r *Request) { r.Cost = 0 }, ErrInvalidCost},
		{"cost above limit", func(r *Request) { r.Cost, r.Limit = 11, 10 }, ErrCostExceedsLimit},
		{"limit zero", func(r *Request) { r.Limit = 0 }, ErrInvalidPolicy},
		{"valid", func(*Request) {}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validRequest()
			tt.mutate(&req)
			if got := req.Validate(); !errors.Is(got, tt.want) {
				t.Fatalf("Validate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNowMillis(t *testing.T) {
	req := validRequest()
	if got, want := req.nowMillis(), int64(1_700_000_000_000); got != want {
		t.Fatalf("nowMillis() = %d, want %d", got, want)
	}
}
