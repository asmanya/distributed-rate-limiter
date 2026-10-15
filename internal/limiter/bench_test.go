package limiter

import "testing"

// sink keeps the compiler from discarding the benchmarked result.
var sink string

func BenchmarkTokenBucketKey(b *testing.B) {
	req := validRequest()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = tokenBucketKey(req)
	}
}

func BenchmarkSlidingWindowKeys(b *testing.B) {
	req := validRequest()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cur, _, _ := slidingWindowKeys(req)
		sink = cur
	}
}
