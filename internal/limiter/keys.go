package limiter

import "strconv"

// baseKey is the Redis key prefix shared by both algorithms. The braces are a
// hash tag: Redis Cluster hashes only what is inside them, so every key of one
// limiter lands in the same slot and a multi-key script can run on a cluster.
func baseKey(r Request) string {
	return "rl:{" + r.Namespace + ":" + r.Policy + ":" + r.Key + "}"
}

// tokenBucketKey is the single HASH key holding a bucket. The ":tb" suffix
// keeps it apart from the sliding window's STRING keys, so a policy that
// switches algorithm on reload never hits WRONGTYPE.
func tokenBucketKey(r Request) string {
	return baseKey(r) + ":tb"
}

// windowOf locates the fixed window containing r.Now. It is the only place the
// window index and start are computed, so keys and script arguments cannot
// disagree about which window they mean.
func windowOf(r Request) (index, startMS int64) {
	periodMS := r.Period.Milliseconds()
	index = r.nowMillis() / periodMS
	return index, index * periodMS
}

// slidingWindowKeys returns the current and previous window keys plus the
// start of the current window in milliseconds.
func slidingWindowKeys(r Request) (current, previous string, startMS int64) {
	index, startMS := windowOf(r)
	base := baseKey(r) + ":sw:"
	return base + strconv.FormatInt(index, 10), base + strconv.FormatInt(index-1, 10), startMS
}
