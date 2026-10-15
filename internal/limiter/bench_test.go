package limiter

import "testing"

// sink keeps the compiler from discarding the benchmarked result.
var sink string

func BenchmarkTokenBucketKey(b *testing.B) {
	req := validRequest()
	b.ReportAllocs()
	for b.Loop() {
		sink = tokenBucketKey(req)
	}
}

func BenchmarkSlidingWindowKeys(b *testing.B) {
	req := validRequest()
	b.ReportAllocs()
	for b.Loop() {
		cur, _, _ := slidingWindowKeys(req)
		sink = cur
	}
}
