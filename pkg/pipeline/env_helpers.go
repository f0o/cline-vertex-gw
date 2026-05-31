package pipeline

import (
	"os"
	"strconv"
	"strings"

	"go.f0o.dev/cline-vertex-gw/pkg/logx"
)

// Centralized environment-variable parsing helpers for the pipeline package.

func envBool(name string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		logx.Warn("invalid env value (want bool); using default",
			"env", name, "value", v, "default", def)
		return def
	}
}

func envInt32(name string, def int32) int32 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 0 {
		logx.Warn("invalid env value (want non-negative int); using default",
			"env", name, "value", v, "default", def)
		return def
	}
	return int32(n)
}

func envString(name string, def string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	return v
}
