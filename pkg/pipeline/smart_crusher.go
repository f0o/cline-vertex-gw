package pipeline

import (
	"encoding/json"
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"log/slog"
	"strings"
)

var logSmartCrusher = logx.Scoped("smart_crusher")

// SmartCrush tries to parse content as JSON or a structured log, and collapses non-critical
// bulk parts while keeping errors, first/last elements, and query-relevant matches.
// This is lossless because the original is cached under retrieve_elided_content before crushing.
func SmartCrush(s string) (string, int) {
	if !smartCrusherEnabled {
		return s, 0
	}
	if len(s) < 64 {
		return s, 0
	}

	// Try JSON crushing first
	if looksLikeJSON(s) {
		if crushed, saved := crushJSON(s); saved > 0 {
			return crushed, saved
		}
	}

	// Fallback to Log crushing if it has multiple lines
	if strings.Count(s, "\n") >= 10 {
		return crushLog(s)
	}

	return s, 0
}

func looksLikeJSON(s string) bool {
	trimmed := strings.TrimSpace(s)
	return (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
		(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]"))
}

func hasErrorTerm(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "error") ||
		strings.Contains(lower, "exception") ||
		strings.Contains(lower, "failed") ||
		strings.Contains(lower, "stderr") ||
		strings.Contains(lower, "fatal") ||
		strings.Contains(lower, "panic") ||
		strings.Contains(lower, "exit status") ||
		strings.Contains(lower, "traceback")
}

func hasErrorDeep(v any) bool {
	switch val := v.(type) {
	case string:
		return hasErrorTerm(val)
	case map[string]any:
		for k, valVal := range val {
			if hasErrorTerm(k) {
				return true
			}
			if hasErrorDeep(valVal) {
				return true
			}
		}
	case []any:
		for _, item := range val {
			if hasErrorDeep(item) {
				return true
			}
		}
	}
	return false
}

// crushJSON walks the parsed JSON structure and collapses oversized arrays that don't contain errors.
func crushJSON(s string) (string, int) {
	var parsed any
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return s, 0
	}

	// Save original to elided cache first in case we do compress it
	hash, err := SaveToElidedCache(s)
	if err != nil {
		logSmartCrusher.Errorf("failed to save JSON to elided cache; skipping compression: %v", err)
		return s, 0
	}

	modified, changed := walkJSON(parsed)
	if !changed {
		return s, 0
	}

	compacted, err := json.Marshal(modified)
	if err != nil {
		return s, 0
	}

	// Prepend/append a metadata tag to let the model know CCR is available
	marker := fmt.Sprintf("\n/* [JSON COMPRESSED - original cached with hash=%s. Use retrieve_elided_content if needed] */\n", hash)
	crushed := marker + string(compacted)
	saved := len(s) - len(crushed)
	if saved <= 0 {
		return s, 0
	}

	logSmartCrusher.L().Debug("successfully crushed JSON payload",
		slog.Int("bytes_saved", saved),
		slog.String("hash", hash),
	)

	return crushed, saved
}

func walkJSON(v any) (any, bool) {
	switch val := v.(type) {
	case map[string]any:
		newMap := make(map[string]any, len(val))
		anyChanged := false
		for k, valVal := range val {
			newVal, changed := walkJSON(valVal)
			newMap[k] = newVal
			if changed {
				anyChanged = true
			}
		}
		return newMap, anyChanged

	case []any:
		if len(val) <= 4 {
			newSlice := make([]any, len(val))
			anyChanged := false
			for i, item := range val {
				newItem, changed := walkJSON(item)
				newSlice[i] = newItem
				if changed {
					anyChanged = true
				}
			}
			return newSlice, anyChanged
		}

		// Keep first 2 and last 2, plus any items with errors in the middle.
		newSlice := make([]any, 0, len(val))
		newSlice = append(newSlice, val[0], val[1])
		elidedCount := 0

		anyChanged := false
		for i := 2; i < len(val)-2; i++ {
			if hasErrorDeep(val[i]) {
				// Flush elided count if we have any accumulated
				if elidedCount > 0 {
					newSlice = append(newSlice, fmt.Sprintf("/* elided %d items */", elidedCount))
					elidedCount = 0
				}
				newItem, _ := walkJSON(val[i])
				newSlice = append(newSlice, newItem)
				anyChanged = true
			} else {
				elidedCount++
				anyChanged = true
			}
		}

		if elidedCount > 0 {
			newSlice = append(newSlice, fmt.Sprintf("/* elided %d items */", elidedCount))
		}

		newSlice = append(newSlice, val[len(val)-2], val[len(val)-1])
		return newSlice, anyChanged
	}

	return v, false
}

// crushLog walks raw lines, fishes out lines with error terms, and collapses sequential clean lines.
func crushLog(s string) (string, int) {
	hash, err := SaveToElidedCache(s)
	if err != nil {
		logSmartCrusher.Errorf("failed to save log to elided cache; skipping log crushing: %v", err)
		return s, 0
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= 12 {
		return s, 0
	}

	keep := make([]bool, len(lines))
	// Always keep the first 3 lines (typically headers/command info) and last 3 lines (typically summary)
	for i := 0; i < 3; i++ {
		keep[i] = true
		keep[len(lines)-1-i] = true
	}

	// Mark any line with an error/exception/etc. to keep
	for i, ln := range lines {
		if hasErrorTerm(ln) {
			// Keep the error line and its surrounding context (1 line before and after for context)
			keep[i] = true
			if i > 0 {
				keep[i-1] = true
			}
			if i < len(lines)-1 {
				keep[i+1] = true
			}
		}
	}

	// Build the crushed log lines
	var crushed []string
	elidedCount := 0
	changed := false

	for i, ln := range lines {
		if keep[i] {
			if elidedCount > 0 {
				crushed = append(crushed, fmt.Sprintf("... [elided %d clean log lines. Retrieve full content: hash=%s] ...", elidedCount, hash))
				elidedCount = 0
				changed = true
			}
			crushed = append(crushed, ln)
		} else {
			elidedCount++
		}
	}

	if elidedCount > 0 {
		crushed = append(crushed, fmt.Sprintf("... [elided %d clean log lines. Retrieve full content: hash=%s] ...", elidedCount, hash))
		changed = true
	}

	if !changed {
		return s, 0
	}

	result := strings.Join(crushed, "\n")
	saved := len(s) - len(result)
	if saved <= 0 {
		return s, 0
	}

	logSmartCrusher.L().Debug("successfully crushed structured log payload",
		slog.Int("bytes_saved", saved),
		slog.String("hash", hash),
	)

	return result, saved
}
