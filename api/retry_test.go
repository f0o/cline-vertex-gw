package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsRetryableError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not retryable", nil, false},
		{"context.Canceled is not retryable", context.Canceled, false},
		{"context.DeadlineExceeded is not retryable", context.DeadlineExceeded, false},
		{"429 retryable", errors.New("got http 429: too many requests"), true},
		{"500 retryable", errors.New("got http 500: internal"), true},
		{"502 retryable", errors.New("upstream returned 502"), true},
		{"503 retryable", errors.New("503 unavailable"), true},
		{"504 retryable", errors.New("504 timeout"), true},
		{"quota retryable", errors.New("Quota exceeded for resource"), true},
		{"unavailable retryable", errors.New("Service Unavailable"), true},
		{"eof retryable", errors.New("unexpected EOF"), true},
		{"connection reset retryable", errors.New("read: connection reset"), true},
		{"401 not retryable", errors.New("got http 401: unauthorized"), false},
		{"403 not retryable", errors.New("permission denied"), false},
		{"404 not retryable", errors.New("not found"), false},
		{"validation not retryable", errors.New("invalid argument"), false},
		// wrapped error must still classify retryable via Error() substring
		{"wrapped 503 retryable", fmt.Errorf("call: %w", errors.New("503 unavailable")), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableError(tt.err); got != tt.want {
				t.Errorf("isRetryableError(%v) = %v; want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestClassifyError(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{context.Canceled, "client-canceled"},
		{context.DeadlineExceeded, "deadline-exceeded"},
		{errors.New("got http 429"), "rate-limited"},
		{errors.New("quota exhausted"), "rate-limited"},
		{errors.New("503 unavailable"), "unavailable"},
		{errors.New("500 internal error"), "upstream-5xx"},
		{errors.New("401 unauthorized"), "auth"},
		{errors.New("permission denied"), "auth"},
		{errors.New("404 not found"), "not-found"},
		{errors.New("connection reset by peer"), "network"},
		{errors.New("unexpected EOF"), "network"},
		{errors.New("xyz garbage"), "other"},
	}
	for _, tt := range tests {
		got := classifyError(tt.err)
		if got != tt.want {
			t.Errorf("classifyError(%v) = %q; want %q", tt.err, got, tt.want)
		}
	}
}

func TestNewReqIDUniqueAndCompact(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := newReqID()
		if len(id) == 0 || len(id) > 32 {
			t.Fatalf("unexpected id length: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id within 1000 calls: %q", id)
		}
		seen[id] = true
	}
}