// Package logx is the gateway's thin leveled-logging facade over log/slog.
//
// # Why this exists
//
// The codebase historically funneled every message through the stdlib `log`
// package, which was bridged into slog at a single fixed INFO level. That made
// LOG_LEVEL filtering meaningless (errors, warnings, and debug detail all
// emitted at INFO) and forced severity to be encoded in the message text
// ("WARNING:", "[pricing][debug]", "error: %v"). logx fixes that by exposing
// real leveled helpers and a structured `component` attribute so logs behave
// like a filterable event stream rather than freeform text.
//
// Design goals
//
//   - Treat logs as an event stream: structured key/value attributes, stable
//     `component` tags, severity carried by the LEVEL (not the text).
//   - Cheap to adopt: a one-line drop-in per call site (logx.Warn(...) instead
//     of log.Printf("WARNING: ...")).
//   - No global state beyond slog's default logger, which main configures once
//     at startup. Packages call logx.For("component") to get a scoped logger.
//
// Level semantics (be deliberate — this is the whole point):
//
//   - Debug: verbose diagnostics useful only when actively investigating. Safe
//     to be noisy; OFF in production by default. Per-item dumps, per-SKU detail,
//     cache hit/miss internals.
//   - Info:  normal lifecycle events an operator wants in steady state. Startup,
//     shutdown, "discovered N models", per-request cost/done summaries.
//   - Warn:  something is wrong but the gateway degraded gracefully and keeps
//     serving. Stale cache used after a refresh failure, invalid env value fell
//     back to a default, a single publisher discovery failed.
//   - Error: an operation failed and produced no usable result / data loss /
//     a request could not be served. Client init failure, cache write failure
//     that loses data, a panic recovered in a handler.
package logx

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

// Default returns the process-wide slog logger. main installs the real handler
// (JSON/text, level from LOG_LEVEL) via slog.SetDefault before any request is
// served; until then this falls back to slog's built-in default.
func Default() *slog.Logger { return slog.Default() }

// For returns a logger scoped to a component, resolving the CURRENT process
// default handler at call time. The component name shows up as a stable
// `component=<name>` attribute on every record, so operators can filter the
// event stream by subsystem (e.g. component=pricing).
//
// IMPORTANT: call For at log time, not at package-init time. slog.Default()
// returns whatever handler is installed *right now*; a logger captured in a
// package-level var before main installs the configured handler (via
// slog.SetDefault) would be frozen at the bootstrap INFO-level handler and
// silently drop DEBUG/level-filtered records. For long-lived component loggers
// use Scoped(...) (see Logger), which resolves the default lazily on each call.
//
// Use short, stable, lower-case names that match the bracketed prefixes the
// codebase already uses in messages: "tags", "pricing", "trim", "dedup",
// "loopbreak", "envblocks", "toolresult", "prune-tools".
func For(component string) *slog.Logger {
	return slog.Default().With(slog.String("component", component))
}

// The following package-level helpers are convenience wrappers for call sites
// that don't already hold a scoped logger. They forward to slog.Default at the
// matching level. Prefer For(component) in hot/structured paths so the
// component attribute is always present.

// Debug logs at DEBUG level. Use for verbose, investigation-only detail.
func Debug(msg string, args ...any) { slog.Default().Debug(msg, args...) }

// Info logs at INFO level. Use for normal lifecycle events.
func Info(msg string, args ...any) { slog.Default().Info(msg, args...) }

// Warn logs at WARN level. Use when the gateway degraded but kept serving.
func Warn(msg string, args ...any) { slog.Default().Warn(msg, args...) }

// Error logs at ERROR level. Use when an operation failed with no usable result.
func Error(msg string, args ...any) { slog.Default().Error(msg, args...) }

// Fatal logs at ERROR level and then exits the process with status 1. Reserve
// this for unrecoverable startup failures (the old log.Fatalf sites). It is a
// distinct helper so the "this terminates the process" intent is explicit at
// the call site rather than hidden behind a stdlib name.
func Fatal(msg string, args ...any) {
	slog.Default().Log(context.Background(), slog.LevelError, msg, args...)
	os.Exit(1)
}

// Logger is a tiny component-scoped wrapper that adds Printf-style helpers on
// top of slog. It exists so the high-frequency, format-string log sites (e.g.
// the per-request compression-pipeline summaries) can migrate to the correct
// LEVEL with a one-token change, without rewriting each message into key/value
// attributes. The message text is preserved as the slog `msg`, and component
// is still attached, so these lines remain filterable by level and component.
//
// It stores only the component NAME and resolves slog.Default() lazily on every
// call. This is the critical property that lets these be declared as
// package-level vars (e.g. `var logTrim = logx.Scoped("trim")`) at import time:
// because the default handler is fetched at log time, such loggers correctly
// pick up the configured handler/level that main installs in configureLogging()
// — they are NOT frozen at the bootstrap INFO-level handler.
//
// Prefer the structured slog API (Logger.L) for new code; the *f helpers are a
// bridge for legacy printf-style messages.
type Logger struct {
	component string
}

// Scoped returns a Logger carrying component=<name> on every record. Safe to
// assign to a package-level var: the underlying handler is resolved lazily.
func Scoped(component string) *Logger {
	return &Logger{component: component}
}

// scoped resolves the CURRENT default handler and attaches the component
// attribute. Cheap (a single With) and, crucially, always reflects the handler
// installed at call time.
func (lg *Logger) scoped() *slog.Logger {
	return slog.Default().With(slog.String("component", lg.component))
}

// L exposes a structured slog.Logger (component attached) for key/value
// logging, resolved against the current default handler.
func (lg *Logger) L() *slog.Logger { return lg.scoped() }

func (lg *Logger) logf(level slog.Level, format string, args ...any) {
	l := lg.scoped()
	// Skip the formatting cost entirely when the level is disabled — these
	// helpers sit on per-request hot paths.
	if !l.Enabled(context.Background(), level) {
		return
	}
	l.Log(context.Background(), level, fmt.Sprintf(format, args...))
}

// Debugf logs a printf-formatted message at DEBUG.
func (lg *Logger) Debugf(format string, args ...any) { lg.logf(slog.LevelDebug, format, args...) }

// Infof logs a printf-formatted message at INFO.
func (lg *Logger) Infof(format string, args ...any) { lg.logf(slog.LevelInfo, format, args...) }

// Warnf logs a printf-formatted message at WARN.
func (lg *Logger) Warnf(format string, args ...any) { lg.logf(slog.LevelWarn, format, args...) }

// Errorf logs a printf-formatted message at ERROR.
func (lg *Logger) Errorf(format string, args ...any) { lg.logf(slog.LevelError, format, args...) }
