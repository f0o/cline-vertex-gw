package pipeline

import (
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"log/slog"

	"google.golang.org/genai"
)

var logWriteElision = logx.Scoped("write-elision")

// ElideHistoricalWriteActions scans historical assistant turns (FunctionCalls)
// for file write/modification calls (specifically write_to_file and replace_in_file).
// If a call's text argument is very large and is older than writeActionRetainWindow turns, it is elided,
// saved to the FSCache, and replaced with a placeholder, saving thousands of tokens.
//
// The original slice and Content/Part/FunctionCall structures are not mutated.
func ElideHistoricalWriteActions(contents []*genai.Content) []*genai.Content {
	if !writeActionElision {
		logWriteElision.Debugf("write action elision is disabled; skipping")
		return contents
	}
	if writeActionRetainWindow <= 0 {
		logWriteElision.Debugf("writeActionRetainWindow <= 0; skipping write action elision")
		return contents
	}
	if len(contents) < 3 {
		logWriteElision.Debugf("history size %d < 3; skipping write action elision", len(contents))
		return contents
	}

	// Exempt the last turn and the one before it (the latest client/assistant interaction)
	lastIdx := -1
	for i := len(contents) - 1; i >= 0; i-- {
		if contents[i] != nil {
			lastIdx = i
			break
		}
	}

	out := make([]*genai.Content, len(contents))
	totalSaved := 0
	elidedCount := 0

	for i, c := range contents {
		distance := int32(lastIdx - i)
		if c == nil || i == lastIdx || distance < writeActionRetainWindow {
			out[i] = c
			continue
		}

		nc := &genai.Content{Role: c.Role}
		if len(c.Parts) > 0 {
			nc.Parts = make([]*genai.Part, len(c.Parts))
		}

		modifiedContent := false

		for j, p := range c.Parts {
			if p == nil {
				nc.Parts[j] = nil
				continue
			}

			if p.FunctionCall == nil {
				nc.Parts[j] = p
				continue
			}

			fc := p.FunctionCall
			if fc.Name != "write_to_file" && fc.Name != "replace_in_file" {
				nc.Parts[j] = p
				continue
			}

			// Determine which argument is the large payload
			argKey := ""
			argLabel := ""
			if fc.Name == "write_to_file" {
				argKey = "content"
				argLabel = "Content"
			} else {
				argKey = "diff"
				argLabel = "Diff"
			}

			val, exists := fc.Args[argKey]
			if !exists {
				nc.Parts[j] = p
				continue
			}

			origString, ok := val.(string)
			// Trigger elision only if the string payload exceeds 2KB
			if !ok || len(origString) <= 2048 {
				nc.Parts[j] = p
				continue
			}

			// Perform elision using standard CCR loop
			hash := SaveToElidedCache(origString)
			placeholder := fmt.Sprintf("[%s: %d bytes written. Elided. Retrieve full content: hash=%s]", argLabel, len(origString), hash)

			// Copy-on-write Part and FunctionCall to avoid mutating input slices
			np := *p
			nfc := *fc
			newArgs := make(map[string]any, len(fc.Args))
			for kk, vv := range fc.Args {
				newArgs[kk] = vv
			}
			newArgs[argKey] = placeholder
			nfc.Args = newArgs
			np.FunctionCall = &nfc

			nc.Parts[j] = &np
			totalSaved += len(origString) - len(placeholder)
			elidedCount++
			modifiedContent = true
		}

		if modifiedContent {
			out[i] = nc
		} else {
			out[i] = c
		}
	}

	if elidedCount > 0 {
		logWriteElision.L().Debug("elided historical write/modification action(s)",
			slog.Int("elided_count", elidedCount),
			slog.Int("bytes_saved", totalSaved),
		)
		onCompressionSaved("write_elision", totalSaved)
	} else {
		logWriteElision.Debugf("no write/modification actions found to elide")
	}

	return out
}
