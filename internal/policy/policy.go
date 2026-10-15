// Package policy loads rate limiting policies from a strict YAML file.
package policy

import (
	"errors"
	"fmt"
	"time"
)

// Algorithm names a rate limiting algorithm. The policy package only knows the
// names; the service maps a name to a limiter, so policy never imports limiter.
type Algorithm string

// The two supported algorithms.
const (
	TokenBucket   Algorithm = "token_bucket"
	SlidingWindow Algorithm = "sliding_window"
)

const maxNameLen = 64

// Policy is one named rate limit.
type Policy struct {
	Name      string        `yaml:"name"`
	Algorithm Algorithm     `yaml:"algorithm"`
	Limit     int64         `yaml:"limit"`
	Period    time.Duration `yaml:"period"`
}

// validate checks every policy and reports all problems at once.
func validate(ps []Policy) error {
	if len(ps) == 0 {
		return fmt.Errorf("%w: at least one policy is required", ErrInvalid)
	}
	var errs []error
	fail := func(i int, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: policy #%d: %s", ErrInvalid, i+1, fmt.Sprintf(format, args...)))
	}

	seen := make(map[string]int, len(ps))
	for i, p := range ps {
		if !validName(p.Name) {
			fail(i, "name %q must be 1-%d characters of a-z, 0-9, '-' or '_'", p.Name, maxNameLen)
		} else if first, dup := seen[p.Name]; dup {
			fail(i, "duplicate name %q (already policy #%d)", p.Name, first+1)
		} else {
			seen[p.Name] = i
		}
		if p.Algorithm != TokenBucket && p.Algorithm != SlidingWindow {
			fail(i, "unknown algorithm %q (want %s or %s)", p.Algorithm, TokenBucket, SlidingWindow)
		}
		if p.Limit <= 0 {
			fail(i, "limit must be positive, got %d", p.Limit)
		}
		// The scripts count in whole milliseconds; 1.5ms means nothing to them.
		if p.Period < time.Millisecond || p.Period%time.Millisecond != 0 {
			fail(i, "period must be a whole number of milliseconds, at least 1ms, got %v", p.Period)
		}
	}
	return errors.Join(errs...)
}

// validName allows only characters that are safe inside a Redis key built as
// namespace:policy:key (no ':', '{' or '}').
func validName(s string) bool {
	if s == "" || len(s) > maxNameLen {
		return false
	}
	for _, r := range s {
		if !isNameChar(r) {
			return false
		}
	}
	return true
}

func isNameChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}
