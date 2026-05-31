package provider

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Centralized environment-variable parsing helpers for the provider package.
//
// Every GW_* knob is read through one of these helpers so the parsing,
// defaulting, and "garbage value → log + default" behavior is uniform. A typo
// in a deployment config should never silently flip a safety net; it logs a
// warning and falls back to the documented default instead.

// envBool parses a boolean env var. Returns def when unset. Accepts
// 1/true/yes/on and 0/false/no/off (case-insensitive). Garbage values log a
// warning and return the default.
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
		log.Printf("invalid %s=%q (want bool); using default %v", name, v, def)
		return def
	}
}

// envInt32 parses a non-negative int32 env var with a default. Logs and
// returns the default on garbage input so a typo can't silently disable a
// safety net.
func envInt32(name string, def int32) int32 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 0 {
		log.Printf("invalid %s=%q (want non-negative int); using default %d", name, v, def)
		return def
	}
	return int32(n)
}

// envDurationSeconds reads an env var holding a count of seconds and returns it
// as a time.Duration. Returns def when the var is unset, empty, non-numeric, or
// negative. Read on each call (not cached) so tests can override via t.Setenv.
func envDurationSeconds(name string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return def
	}
	return time.Duration(n) * time.Second
}
