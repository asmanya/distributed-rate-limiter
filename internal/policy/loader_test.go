package policy

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const validYAML = `
policies:
  - name: search
    algorithm: token_bucket
    limit: 100
    period: 1m
  - name: login
    algorithm: sliding_window
    limit: 5
    period: 10s
  - name: greeter
    algorithm: token_bucket
    limit: 5
    period: 10s
`

func TestParseValid(t *testing.T) {
	got, err := Parse(strings.NewReader(validYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []Policy{
		{"search", TokenBucket, 100, time.Minute},
		{"login", SlidingWindow, 5, 10 * time.Second},
		{"greeter", TokenBucket, 5, 10 * time.Second},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d policies, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("policy %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseInvalid(t *testing.T) {
	policy := func(name, algo, limit, period string) string {
		return "policies:\n  - name: " + name + "\n    algorithm: " + algo +
			"\n    limit: " + limit + "\n    period: " + period + "\n"
	}
	tests := []struct {
		name string
		yaml string
		want string // substring of the error; "" means only ErrInvalid is checked
	}{
		{"duplicate name", policy("a", "token_bucket", "5", "1s") +
			"  - name: a\n    algorithm: token_bucket\n    limit: 5\n    period: 1s\n", "duplicate"},
		{"unknown algorithm", policy("a", "leaky_bucket", "5", "1s"), "algorithm"},
		{"limit zero", policy("a", "token_bucket", "0", "1s"), "limit"},
		{"unknown field", "policies:\n  - name: a\n    algorithm: token_bucket\n    limt: 5\n    period: 1s\n", ""},
		{"period not a duration", policy("a", "token_bucket", "5", "abc"), ""},
		{"period without a unit", policy("a", "token_bucket", "5", "60"), ""},
		{"period not whole milliseconds", policy("a", "token_bucket", "5", "1500us"), "period"},
		{"invalid name", policy("Search", "token_bucket", "5", "1s"), "name"},
		{"empty file", "", "empty"},
		{"no policies", "policies: []\n", "at least one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.yaml))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if tt.want != "" && !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestParseReportsAllErrorsAtOnce(t *testing.T) {
	_, err := Parse(strings.NewReader(`
policies:
  - name: a
    algorithm: bogus
    limit: 5
    period: 1s
  - name: b
    algorithm: token_bucket
    limit: 0
    period: 1s
`))
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"policy #1", "algorithm", "policy #2", "limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, missing %q", err, want)
		}
	}
}

// The file we ship must itself be valid.
func TestShippedPolicies(t *testing.T) {
	got, err := LoadFile("../../deploy/policies.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "search,login,greeter" {
		t.Fatalf("names = %v, want search, login, greeter", names)
	}
}
