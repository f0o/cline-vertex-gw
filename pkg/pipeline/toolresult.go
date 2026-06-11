package pipeline

import (
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"log/slog"
	"strings"

	"google.golang.org/genai"
)

// logToolresult scopes pipeline-compression logs to component=toolresult (DEBUG: per-request diagnostics).
var logToolresult = logx.Scoped("toolresult")

// Tool results (read_file dumps, terminal output, search results) dominate
// token volume in agentic Cline sessions. The same enormous blob is often
// only needed in full at the moment it was produced; on OLDER turns the
// model rarely needs every line of a 4,000-line file it inspected ten turns
// ago — it needs to remember that it looked, and roughly what it found.
//
// This compressor middle-elides oversized tool-result content on every turn
// EXCEPT the most recent one, keeping a head and tail window and dropping the
// middle. Keeping both ends preserves the most useful context (file headers,
// imports, and the trailing summary/error lines) while shedding the bulk.
//
//   - GW_TOOL_RESULT_TRUNCATE (default: on)
//     Master switch. Set to {0,false,off,no} to disable.
//   - GW_TOOL_RESULT_MAX_BYTES (default: 8000)
//     Only tool-result text larger than this is eligible. ~2.3k tokens.
//   - GW_TOOL_RESULT_HEAD_BYTES (default: 2000)
//     Bytes preserved from the start of an elided block.
//   - GW_TOOL_RESULT_TAIL_BYTES (default: 1000)
//     Bytes preserved from the end of an elided block.
//
// Conservative on purpose:
//   - The LATEST turn is always left intact — the freshest tool output is the
//     one the model is most likely reasoning about right now.
//   - Only triggers when head+tail+placeholder is meaningfully smaller than
//     the original; otherwise truncation is a net loss and we skip it.
//   - Both genai FunctionResponse parts AND plain text parts that look like
//     tool output (the Ollama/OpenAI dialects flatten tool results into text)
//     are handled, so the win is uniform across surfaces.
//
// Like the other compressors this runs at the dispatch layer so all
// publishers benefit uniformly.

// TruncateToolResults returns a copy of contents in which oversized
// tool-result text on every turn except the last is middle-elided to a
// head+tail window. The original slice and Content objects are not mutated.
//
// When GW_TOOL_RESULT_TRUNCATE is off this is a fast-path no-op.
//
// Should run BEFORE TrimContents so the byte budget is computed against the
// already-shrunk sizes (truncation may make trimming unnecessary), and
// BEFORE DedupReplayedBlocks so dedup hashes the post-truncation bodies.
func TruncateToolResults(contents []*genai.Content) []*genai.Content {
	if !toolResultTruncate || len(contents) < 2 {
		return contents
	}

	// The last non-nil turn is exempt — keep the freshest tool output whole.
	lastIdx := -1
	for i := len(contents) - 1; i >= 0; i-- {
		if contents[i] != nil {
			lastIdx = i
			break
		}
	}

	out := make([]*genai.Content, len(contents))
	totalSaved := 0
	truncatedCount := 0
	for i, c := range contents {
		if c == nil || i == lastIdx {
			out[i] = c
			continue
		}

		distance := int32(lastIdx - i)
		if toolResultRetainWindow > 0 && distance < toolResultRetainWindow {
			out[i] = c
			continue
		}

		// Calculate distance to determine if progressive masking should be aggressive.
		aggressive := false
		if toolResultRetainWindow > 0 {
			if distance >= toolResultRetainWindow + 2 {
				aggressive = true
			}
		}

		nc := &genai.Content{Role: c.Role}
		if len(c.Parts) > 0 {
			nc.Parts = make([]*genai.Part, len(c.Parts))
		}
		for j, p := range c.Parts {
			if p == nil {
				nc.Parts[j] = nil
				continue
			}
			// FunctionResponse parts: truncate the textual payload carried
			// in the response map under common keys ("output"/"content"/
			// "result"/"text"); fall back to plain text parts below.
			if p.FunctionResponse != nil {
				np, saved := truncateFunctionResponse(p, aggressive)
				nc.Parts[j] = np
				if saved > 0 {
					totalSaved += saved
					truncatedCount++
				}
				continue
			}
			if p.Text == "" || int32(len(p.Text)) <= toolResultMaxBytes {
				np := *p
				nc.Parts[j] = &np
				continue
			}
			newText, saved := elideString(p.Text, aggressive)
			np := *p
			np.Text = newText
			nc.Parts[j] = &np
			if saved > 0 {
				totalSaved += saved
				truncatedCount++
			}
		}
		out[i] = nc
	}
	if truncatedCount > 0 {
		logToolresult.L().Debug("truncated oversized tool result(s)",
			slog.Int("truncated_count", truncatedCount),
			slog.Int("bytes_saved", totalSaved),
		)
		onCompressionSaved("toolresult", totalSaved)
	}
	return out
}

// toolResultTextKeys are the FunctionResponse map keys whose string values
// hold the bulky tool output. We only elide string values under these keys
// so we never corrupt structured (numeric/nested) response fields.
var toolResultTextKeys = []string{"output", "content", "result", "text", "stdout"}

// truncateFunctionResponse returns a copy of a FunctionResponse part with any
// oversized string payload under a known text key middle-elided or aggressively elided, plus the
// bytes saved. The original part and its Response map are not mutated.
func truncateFunctionResponse(p *genai.Part, aggressive bool) (*genai.Part, int) {
	fr := p.FunctionResponse
	if fr == nil || len(fr.Response) == 0 {
		np := *p
		return &np, 0
	}
	saved := 0
	var newResp map[string]any
	for _, k := range toolResultTextKeys {
		v, ok := fr.Response[k]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok || int32(len(s)) <= toolResultMaxBytes {
			continue
		}
		newText, s2 := elideString(s, aggressive)
		if s2 <= 0 {
			continue
		}
		if newResp == nil {
			// Copy-on-write the response map so the caller's copy is intact.
			newResp = make(map[string]any, len(fr.Response))
			for kk, vv := range fr.Response {
				newResp[kk] = vv
			}
		}
		newResp[k] = newText
		saved += s2
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

// middleElide keeps the first toolResultHeadBytes and last toolResultTailBytes
// of s, replacing the middle with a one-line marker. Returns the rewritten
// string and the number of bytes saved (0 if truncation wasn't worthwhile or
// the cut points landed on invalid UTF-8 boundaries that we couldn't fix
// cheaply). Cut points are nudged to the nearest newline within a small
// window so we don't slice through the middle of a line.
func middleElide(s string) (string, int) {
	head := int(toolResultHeadBytes)
	tail := int(toolResultTailBytes)
	if head < 0 {
		head = 0
	}
	if tail < 0 {
		tail = 0
	}
	// Nothing worth doing if the window already covers the whole string.
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
	// Save original uncompacted text to cache before elision.
	hash := SaveToElidedCache(s)
	marker := fmt.Sprintf("\n\n… %d bytes elided (tool result truncated for older turn). Retrieve full content: hash=%s …\n\n", elided, hash)
	// Only proceed if we actually save more than the marker costs.
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

// completeElide aggressively compresses the entire tool result string s,
// saving it to FSCache and returning a compact tombstone containing its SHA-256 hash.
func completeElide(s string) (string, int) {
	hash := SaveToElidedCache(s)
	marker := fmt.Sprintf("[Tool output masked: %d bytes elided. Retrieve full content: hash=%s]", len(s), hash)
	if len(s) <= len(marker) {
		return s, 0
	}
	return marker, len(s) - len(marker)
}

// elideString routing helper selects between middleElide and completeElide.
func elideString(s string, aggressive bool) (string, int) {
	if aggressive {
		return completeElide(s)
	}
	return middleElide(s)
}

// snapToNewline nudges byte offset idx to a nearby '\n' boundary (within a
// 256-byte window) so elision cuts land on line boundaries instead of mid-
// line. When forward is true it searches ahead (used for the head cut);
// otherwise it searches backward (used for the tail cut). If no newline is
// found in the window, idx is returned unchanged.
func snapToNewline(s string, idx int, forward bool) int {
	if idx <= 0 {
		return 0
	}
	if idx >= len(s) {
		return len(s)
	}
	const window = 256
	if forward {
		end := idx + window
		if end > len(s) {
			end = len(s)
		}
		if nl := strings.IndexByte(s[idx:end], '\n'); nl >= 0 {
			return idx + nl + 1
		}
		return idx
	}
	start := idx - window
	if start < 0 {
		start = 0
	}
	if nl := strings.LastIndexByte(s[start:idx], '\n'); nl >= 0 {
		return start + nl
	}
	return idx
}
