package main

import (
	"log/slog"
	"testing"
)

// TestInferLevel locks in the severity classification used by the stdlib-log
// bridge so legacy/third-party `log.Printf` lines map to the right slog level
// instead of all collapsing to INFO.
func TestInferLevel(t *testing.T) {
	cases := []struct {
		msg  string
		want slog.Level
	}{
		{"[pricing][debug] sku desc=foo", slog.LevelDebug},
		{"SECURITY WARNING: bound without auth", slog.LevelWarn},
		{"WARNING: project not set", slog.LevelWarn},
		{"invalid PORT=abc; using default 11434", slog.LevelWarn},
		{"using stale on-disk cache after failure", slog.LevelWarn},
		{"invalid GW_FOO=x (want bool); using default true", slog.LevelWarn},
		{"PANIC handling GET /api/tags", slog.LevelError},
		{"writing on-disk cache failed: disk full", slog.LevelError},
		{"publisher=x endpoint=y error: boom", slog.LevelError},
		{"Starting Cline Vertex Gateway version=dev", slog.LevelInfo},
		{"discovered 42 models live", slog.LevelInfo},
	}
	for _, c := range cases {
		if got := inferLevel(c.msg); got != c.want {
			t.Errorf("inferLevel(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}
