package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

	"go.f0o.dev/cline-vertex-gw/pkg/logx"
)

// retryConfig controls the exponential backoff retry behavior used by the
// chat and generate handlers when talking to Vertex AI.
type retryConfig struct {
	MaxAttempts int           // Total attempts including the first one.
	BaseDelay   time.Duration // Delay before the second attempt.
	MaxDelay    time.Duration // Upper bound on a single sleep.
}

// defaultRetry returns the project-wide default retry policy.
func defaultRetry() retryConfig {
	return retryConfig{
		MaxAttempts: 4,
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    8 * time.Second,
	}
}

// waitFor sleeps for an exponentially growing, jittered delay before the
// (attempt+1)-th retry. attempt is zero-based: 0 == before the second try.
// It returns early (with ctx.Err()) if the context is cancelled.
func (rc retryConfig) waitFor(ctx context.Context, attempt int) error {
	delay := rc.BaseDelay << attempt
	if delay <= 0 || delay > rc.MaxDelay {
		delay = rc.MaxDelay
	}
	// Add up to 25% jitter to avoid thundering-herd retries.
	if delay > 4 {
		jitter := time.Duration(rand.Int63n(int64(delay) / 4))
		delay += jitter
	}
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isRetryableError reports whether the upstream Vertex AI error is worth
// retrying. It deliberately avoids retrying client cancellations or 4xx
// errors that aren't rate-limit related.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	errStr := strings.ToLower(err.Error())
	retryableSubstrings := []string{
		"429",
		"500", "502", "503", "504",
		"quota",
		"exhaust",
		"overloaded",
		"unavailable",
		"connection reset",
		"connection refused",
		"broken pipe",
		"eof",
		"temporarily",
		"timeout",
		"internal error",
	}
	for _, sub := range retryableSubstrings {
		if strings.Contains(errStr, sub) {
			return true
		}
	}
	return false
}

// classifyError returns a short tag for logging based on err contents,
// helping debug whether failures cluster around a particular cause.
func classifyError(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) {
		return "client-canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline-exceeded"
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "429"), strings.Contains(s, "quota"), strings.Contains(s, "exhaust"):
		return "rate-limited"
	case strings.Contains(s, "overloaded"):
		return "overloaded"
	case strings.Contains(s, "503"), strings.Contains(s, "unavailable"):
		return "unavailable"
	case strings.Contains(s, "500"), strings.Contains(s, "internal"):
		return "upstream-5xx"
	case strings.Contains(s, "401"), strings.Contains(s, "403"), strings.Contains(s, "permission"):
		return "auth"
	case strings.Contains(s, "404"), strings.Contains(s, "not found"):
		return "not-found"
	case strings.Contains(s, "connection"), strings.Contains(s, "eof"), strings.Contains(s, "broken"):
		return "network"
	default:
		return "other"
	}
}

// reqIDCounter monotonically increases for each request; combined with a
// time-prefix it yields a short, unique, sortable ID useful for log grepping.
var reqIDCounter uint64

// newReqID returns a short unique-ish identifier suitable for prefixing logs.
func newReqID() string {
	return fmt.Sprintf("%06x-%03x",
		time.Now().UnixNano()&0xFFFFFF,
		atomic.AddUint64(&reqIDCounter, 1)&0xFFF,
	)
}

// reqLogger emits per-request log records as a structured event stream. The
// stable request ID and route ride along as slog attributes (req=<id>,
// route=<name>) on every line — not baked into the message text — so an
// aggregator can group an entire request's lifecycle by req and filter by
// route. Each helper carries the correct LEVEL for its semantics.
type reqLogger struct {
	id    string
	route string
	start time.Time
	l     *slog.Logger
}

func newReqLogger(route string) *reqLogger {
	id := newReqID()
	return &reqLogger{
		id:    id,
		route: route,
		start: time.Now(),
		l:     logx.For("request").With(slog.String("req", id), slog.String("route", route)),
	}
}

// L returns the request-scoped structured logger (req + route attributes
// already attached) for call sites that want to log explicit key/value pairs.
func (rl *reqLogger) L() *slog.Logger { return rl.l }

// Logf logs request progress at INFO. Use for normal lifecycle/progress lines
// (request accepted, served N models, first chunk, retry succeeded).
func (rl *reqLogger) Logf(format string, args ...any) {
	rl.l.Info(fmt.Sprintf(format, args...))
}

// Warnf logs a recoverable problem at WARN — the request continued or was
// retried (a retry attempt, a partial-output abort that may still recover).
func (rl *reqLogger) Warnf(format string, args ...any) {
	rl.l.Warn(fmt.Sprintf(format, args...))
}

// Errorf logs a request-fatal problem at ERROR — the request could not be
// served (client not configured, body too large, parse failure, non-retryable
// upstream error, encode failure).
func (rl *reqLogger) Errorf(format string, args ...any) {
	rl.l.Error(fmt.Sprintf(format, args...))
}

func (rl *reqLogger) Elapsed() time.Duration {
	return time.Since(rl.start)
}
