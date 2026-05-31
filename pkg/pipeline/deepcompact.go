package pipeline

import (
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"log/slog"
	"strings"

	"google.golang.org/genai"
)

// logDeepcompact scopes logs to component=deepcompact.
var logDeepcompact = logx.Scoped("deepcompact")

// Configuration for Deep Compaction.
var (
	deepCompactEnabled   = envBool("GW_DEEP_COMPACT", false)
	deepCompactKeepTurns = envInt32("GW_DEEP_COMPACT_KEEP_TURNS", 12)
	deepCompactMaxBytes  = envInt32("GW_DEEP_COMPACT_MAX_BYTES", 500)
	deepCompactHeadBytes = envInt32("GW_DEEP_COMPACT_HEAD_BYTES", 200)
	deepCompactTailBytes = envInt32("GW_DEEP_COMPACT_TAIL_BYTES", 100)
)

// DeepCompactHistoricalTurns processes older turns (outside of the active/warm window)
// and aggressively compresses any large text content or tool outputs (FunctionResponse),
// reducing them from several kilobytes down to tiny high-density placeholders.
// This allows long-running agentic workloads to run up to 100+ turns without blowing
// through prompt budgets or sacrificing system prompts/recent context.
func DeepCompactHistoricalTurns(contents []*genai.Content) []*genai.Content {
	if !deepCompactEnabled || len(contents) <= int(deepCompactKeepTurns) {
		return contents
	}

	out := make([]*genai.Content, len(contents))
	// The warm window is the last deepCompactKeepTurns messages.
	// Index boundaries: [0, coldLimit) is the cold historical turns zone.
	coldLimit := len(contents) - int(deepCompactKeepTurns)
	totalSaved := 0
	compactedCount := 0

	for i, c := range contents {
		if c == nil {
			continue
		}
		if i >= coldLimit {
			// Inside the warm window, leave the turn completely unchanged.
			out[i] = c
			continue
		}

		// Cold turn compaction: deep-compact large text/tool parts.
		nc := *c
		nc.Parts = make([]*genai.Part, len(c.Parts))
		for j, p := range c.Parts {
			if p == nil {
				continue
			}

			// 1. If it's a FunctionResponse, do deep tool output compression.
			if p.FunctionResponse != nil {
				newPart, saved := deepCompactFunctionResponse(p)
				nc.Parts[j] = newPart
				if saved > 0 {
					totalSaved += saved
					compactedCount++
				}
				continue
			}

			// 2. If it's a plain text block, do deep text compression.
			if p.Text != "" {
				if int32(len(p.Text)) <= deepCompactMaxBytes {
					np := *p
					nc.Parts[j] = &np
					continue
				}
				newText, saved := deepMiddleElide(p.Text)
				np := *p
				np.Text = newText
				nc.Parts[j] = &np
				if saved > 0 {
					totalSaved += saved
					compactedCount++
				}
				continue
			}

			// Leave other part types (blobs, etc.) untouched.
			nc.Parts[j] = p
		}
		out[i] = &nc
	}

	if compactedCount > 0 {
		logDeepcompact.L().Debug("deeply compacted cold historical turn payload(s)",
			slog.Int("compacted_count", compactedCount),
			slog.Int("bytes_saved", totalSaved),
		)
		onCompressionSaved("deepcompact", totalSaved)
	}

	return out
}

// deepCompactFunctionResponse compresses known bulky fields in historical FunctionResponse parts semantically.
func deepCompactFunctionResponse(p *genai.Part) (*genai.Part, int) {
	fr := p.FunctionResponse
	if fr == nil || len(fr.Response) == 0 {
		np := *p
		return &np, 0
	}
	saved := 0
	var newResp map[string]any

	isSemanticTool := false
	var semanticSummary string
	switch strings.ToLower(fr.Name) {
	case "read_file", "read_file_result", "view_file":
		isSemanticTool = true
		filename := "file"
		if pathVal, ok := fr.Response["path"]; ok {
			if s, ok := pathVal.(string); ok {
				filename = s
			}
		} else if fileVal, ok := fr.Response["file"]; ok {
			if s, ok := fileVal.(string); ok {
				filename = s
			}
		}
		semanticSummary = fmt.Sprintf("[Stale tool result: read file '%s' successfully (file content elided for historical compaction)]", filename)
	case "execute_command", "run_command", "run_terminal_command":
		isSemanticTool = true
		semanticSummary = "[Stale tool result: shell command completed (console stdout/stderr elided for historical compaction)]"
	case "web_search", "google_search", "google_search_retrieval", "search_web":
		isSemanticTool = true
		semanticSummary = "[Stale tool result: web query completed successfully (raw search results elided for historical compaction)]"
	case "web_fetch", "fetch_web_page", "view_web_page":
		isSemanticTool = true
		semanticSummary = "[Stale tool result: web page fetch completed (raw HTML elided for historical compaction)]"
	}

	if isSemanticTool {
		newResp = make(map[string]any, len(fr.Response))
		for kk, vv := range fr.Response {
			newResp[kk] = vv
		}
		for _, k := range toolResultTextKeys {
			if _, ok := fr.Response[k]; ok {
				if origStr, ok := fr.Response[k].(string); ok {
					saved += len(origStr) - len(semanticSummary)
				}
				newResp[k] = semanticSummary
			}
		}
	} else {
		for _, k := range toolResultTextKeys {
			v, ok := fr.Response[k]
			if !ok {
				continue
			}
			s, ok := v.(string)
			if !ok || int32(len(s)) <= deepCompactMaxBytes {
				continue
			}
			newText, s2 := deepMiddleElide(s)
			if s2 <= 0 {
				continue
			}
			if newResp == nil {
				newResp = make(map[string]any, len(fr.Response))
				for kk, vv := range fr.Response {
					newResp[kk] = vv
				}
			}
			newResp[k] = newText
			saved += s2
		}
	}

	if newResp == nil {
		np := *p
		return &np, 0
	}
	np := *p
	nfr := *fr
	nfr.Response = newResp
	np.FunctionResponse = &nfr
	return &np, saved
}

// deepMiddleElide middle-elides a string using smaller cold-turn bounds.
func deepMiddleElide(s string) (string, int) {
	head := int(deepCompactHeadBytes)
	tail := int(deepCompactTailBytes)
	if head < 0 {
		head = 0
	}
	if tail < 0 {
		tail = 0
	}
	if head+tail >= len(s) {
		return s, 0
	}

	headEnd := snapToNewline(s, head, true)
	tailStart := len(s) - tail
	tailStart = snapToNewline(s, tailStart, false)
	if tailStart <= headEnd {
		return s, 0
	}

	elided := tailStart - headEnd
	marker := fmt.Sprintf("\n\n… %d bytes elided (stale history deeply compacted) …\n\n", elided)
	if elided <= len(marker) {
		return s, 0
	}

	var b strings.Builder
	b.Grow(headEnd + len(marker) + (len(s) - tailStart))
	b.WriteString(s[:headEnd])
	b.WriteString(marker)
	b.WriteString(s[tailStart:])
	return b.String(), elided - len(marker)
}
