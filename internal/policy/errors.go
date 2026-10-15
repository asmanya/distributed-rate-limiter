package policy

import "errors"

// ErrInvalid is wrapped by every failure to read or validate a policy file.
var ErrInvalid = errors.New("invalid policies")
