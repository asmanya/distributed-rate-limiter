package limiter

import "errors"

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrInvalidKey means the key is empty, too long, or contains '{' or '}'.
	ErrInvalidKey = errors.New("invalid key")

	// ErrInvalidName means the namespace or policy is empty, or contains
	// ':', '{' or '}', which would make two different limiters share a key.
	ErrInvalidName = errors.New("invalid name")

	// ErrInvalidCost means cost is not positive. Turning 0 into 1 is the
	// service's job, not the limiter's.
	ErrInvalidCost = errors.New("invalid cost")

	// ErrCostExceedsLimit means cost is larger than the limit, so the request
	// could never pass however long the caller waits. This is a caller bug,
	// not a denial.
	ErrCostExceedsLimit = errors.New("cost exceeds limit")

	// ErrInvalidPolicy means the limit or period is not positive.
	ErrInvalidPolicy = errors.New("invalid policy params")

	// ErrStoreUnavailable means Redis failed or timed out. The underlying
	// error is wrapped so callers can check errors.Is without importing the
	// Redis package.
	ErrStoreUnavailable = errors.New("store unavailable")
)
