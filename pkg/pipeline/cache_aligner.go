package pipeline

import (
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"strings"
)

var logCacheAligner = logx.Scoped("cache_aligner")

// AlignSystemPromptCache stabilizes the prefix of the system prompt by isolating
// volatile, frequently-changing lines (such as timestamps, date/time statements,
// or session IDs) and relocating them to the very end of the system instructions.
// This preserves KV-cache prefix hits for the massive static instructions and tool schemas.
func AlignSystemPromptCache(s string) string {
	if !cacheAlignerEnabled {
		logCacheAligner.Debugf("system prompt cache aligner is disabled; skipping")
		return s
	}
	if s == "" {
		logCacheAligner.Debugf("system prompt is empty; skipping cache alignment")
		return s
	}

	lines := strings.Split(s, "\n")
	var staticLines []string
	var volatileLines []string

	// Common prefixes that carry volatile, transient info
	volatilePrefixes := []string{
		"current date & time:",
		"current date:",
		"current time:",
		"date & time:",
		"current working directory:",
		"working directory:",
		"session id:",
		"request id:",
	}

	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		lower := strings.ToLower(trimmed)
		isVolatile := false

		for _, pfx := range volatilePrefixes {
			if strings.HasPrefix(lower, pfx) {
				isVolatile = true
				break
			}
		}

		if isVolatile {
			volatileLines = append(volatileLines, ln)
		} else {
			staticLines = append(staticLines, ln)
		}
	}

	// If no volatile lines were found, return original string unchanged
	if len(volatileLines) == 0 {
		logCacheAligner.Debugf("no volatile context lines found in system prompt; skipping cache alignment")
		return s
	}

	// Re-assemble: static prefix first, then volatile suffix
	var sb strings.Builder
	sb.Grow(len(s) + 128)

	sb.WriteString(strings.Join(staticLines, "\n"))
	sb.WriteString("\n\n=== Volatile Runtime Context (Stabilized at Suffix for Prefix Caching) ===\n")
	sb.WriteString(strings.Join(volatileLines, "\n"))

	logCacheAligner.Debugf("relocated %d volatile context line(s) to system prompt suffix to stabilize prefix cache", len(volatileLines))
	return sb.String()
}
