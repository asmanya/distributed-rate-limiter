package limiter

import (
	"strconv"
	"testing"
	"time"
)

func TestFormatRate(t *testing.T) {
	tests := []struct {
		name   string
		limit  int64
		period time.Duration
	}{
		{"ten per ten seconds", 10, 10 * time.Second},
		{"one per hour", 1, time.Hour},
		{"one per millisecond", 1, time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validRequest()
			req.Limit, req.Period = tt.limit, tt.period

			got := formatRate(req)
			parsed, err := strconv.ParseFloat(got, 64)
			want := float64(tt.limit) / float64(tt.period.Milliseconds())
			if err != nil || parsed != want || parsed == 0 {
				t.Fatalf("formatRate() = %q (parsed %v, err %v), want %v and non-zero", got, parsed, err, want)
			}
		})
	}

	t.Run("shortest form", func(t *testing.T) {
		req := validRequest()
		req.Limit, req.Period = 10, 10*time.Second
		if got := formatRate(req); got != "0.001" {
			t.Fatalf("formatRate() = %q, want 0.001", got)
		}
	})
}

func TestDecisionFromReply(t *testing.T) {
	tests := []struct {
		name    string
		reply   []int64
		want    Decision
		wantErr bool
	}{
		{"allowed", []int64{1, 9, 0, 1000},
			Decision{Allowed: true, Limit: 10, Remaining: 9, ResetAfter: time.Second}, false},
		{"denied", []int64{0, 0, 1000, 3000},
			Decision{Limit: 10, RetryAfter: time.Second, ResetAfter: 3 * time.Second}, false},
		{"too few values", []int64{1, 9, 0}, Decision{}, true},
		{"too many values", []int64{1, 9, 0, 1000, 5}, Decision{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decisionFromReply(10, tt.reply)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
