package policy

import (
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

// Parse reads policies from r and validates them. Unknown fields are an error:
// without that, a typo like "limt" would be silently ignored.
func Parse(r io.Reader) ([]Policy, error) {
	var doc struct {
		Policies []Policy `yaml:"policies"`
	}
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) { // an empty file decodes to EOF
			return nil, fmt.Errorf("%w: file is empty", ErrInvalid)
		}
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := validate(doc.Policies); err != nil {
		return nil, err
	}
	return doc.Policies, nil
}

// LoadFile opens path and parses it with Parse.
func LoadFile(path string) ([]Policy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	defer func() { _ = f.Close() }() // read-only: a failed close loses nothing
	return Parse(f)
}
