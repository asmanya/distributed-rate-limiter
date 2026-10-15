package config

import "errors"

// ErrInvalidConfig is wrapped by every validation failure, so callers can test
// errors.Is(err, ErrInvalidConfig) on the joined error Load returns.
var ErrInvalidConfig = errors.New("invalid config")
