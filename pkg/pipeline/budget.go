package pipeline

import (
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"log/slog"
	"strings"

	"google.golang.org/genai"
)

// logTrim scopes pipeline-compression logs to component=trim (DEBUG: per-request diagnostics).
var logTrim = logx.Scoped("trim")

// Conversation-history trimming knobs. Cline sessions accumulate large
// tool-result blobs in older turns; on long-running tasks the request body
// can exceed a model's context window or simply waste tokens on stale data.
//
//   - GW_MAX_INPUT_CHARS (default: 0 = unset)
//     Soft byte-budget on the combined size of all messages + system prompt
//     passed to the upstream. When set and exceeded, oldest non-system turns
//     are dropped first (in pairs to keep user/assistant alternation valid)
//     until the body fits. The LAST user turn is never dropped — it's the
//     question we're actually answering.
//
//     We use a char count instead of a true token count because real
//     tokenization would require per-publisher tokenizers (tiktoken, Claude
//     bpe, etc.) we don't want to bundle. Approximate ratio: ~3.5 chars/token
//     for English prose, so GW_MAX_INPUT_CHARS=350000 ≈ 100k tokens.
//
// 0 disables trimming entirely (default). Setting this should be safe even
// with prompt caching on: the cache prefix may be invalidated after a trim
// but will warm again on the next request that fits.
var maxInputChars = envInt32("GW_MAX_INPUT_CHARS", 0)

// minRetainedTurns is the floor on how many recent messages we'll keep even
// when nothing fits the budget. Without this, an oversized single message
// would be dropped and the call would fail with empty contents.
const minRetainedTurns = 1

// TrimContents drops the oldest non-system messages until the combined size
// of contents + systemPrompt is within the configured byte budget, or until
// only minRetainedTurns remain (whichever happens first). The system prompt
// is never trimmed — operators are expected to manage its size separately
// via GW_DEFAULT_MAX_OUTPUT_TOKENS and prompt-engineering decisions.
//
// Returns the (possibly shorter) slice of contents to forward. The original
// slice is never mutated.
//
// When this trims at least one turn it logs at INFO level so operators can
// see when their budget is too aggressive (e.g. if it fires every request,
// raise GW_MAX_INPUT_CHARS).
func TrimContents(contents []*genai.Content, systemPrompt string) []*genai.Content {
	if maxInputChars == 0 || len(contents) <= minRetainedTurns {
		return contents
	}

	budget := int(maxInputChars)
	sysBytes := len(strings.TrimSpace(systemPrompt))
	if sysBytes >= budget {
		// System prompt alone exceeds budget. Nothing the trimmer can do
		// without breaking the system instruction; just return last turn.
		logTrim.Debugf("system prompt (%dB) >= GW_MAX_INPUT_CHARS (%d); keeping only last turn",
			sysBytes, budget)
		return contents[len(contents)-minRetainedTurns:]
	}
	remaining := budget - sysBytes

	// Compute per-turn byte sizes once.
	sizes := make([]int, len(contents))
	total := 0
	for i, c := range contents {
		sizes[i] = contentBytes(c)
		total += sizes[i]
	}
	if total <= remaining {
		return contents // Already fits.
	}

	// Walk from the END (newest first) accumulating turns into the kept set
	// until we'd exceed the budget. This guarantees we keep the most recent
	// context, including the final user message that's the actual question.
	keepFromIdx := len(contents) // exclusive lower bound of the kept slice
	used := 0
	for i := len(contents) - 1; i >= 0; i-- {
		if used+sizes[i] > remaining && len(contents)-i > minRetainedTurns {
			break
		}
		used += sizes[i]
		keepFromIdx = i
	}

	if keepFromIdx == 0 {
		return contents // Nothing to drop (defensive — shouldn't happen here).
	}

	dropped := keepFromIdx
	kept := contents[keepFromIdx:]
	totalSaved := total - used
	logTrim.L().Debug("trimmed oldest turns to fit budget",
		slog.Int("budget", budget),
		slog.Int("sys_bytes", sysBytes),
		slog.Int("total_bytes", total),
		slog.Int("dropped_turns", dropped),
		slog.Int("kept_turns", len(kept)),
		slog.Int("used_bytes", used),
		slog.Int("bytes_saved", totalSaved),
	)
	if totalSaved > 0 {
		onCompressionSaved("trim", totalSaved)
	}
	return kept
}

// contentBytes returns an approximate byte count for one genai.Content.
// Text parts contribute their literal byte length; image parts (InlineData)
// contribute their decoded byte length — since per-image upload bandwidth
// and per-image token cost both scale with decoded size, that's the right
// budget signal. FunctionCall / FunctionResponse parts contribute a flat
// 64 bytes (rough placeholder; their serialized JSON is usually tiny
// compared to either text or image parts).
//
// Note: this is approximate by design — Vertex's actual token accounting
// for images is publisher-specific (Gemini bills per tile, Claude per
// 750-pixel-axis chunk, etc.). A byte-count proxy is close enough for
// trim-budget decisions and avoids bundling per-publisher tokenizers.
// ContentBytes is the exported version of contentBytes for other packages (like cache).
func ContentBytes(c *genai.Content) int {
	return contentBytes(c)
}

func contentBytes(c *genai.Content) int {
	if c == nil {
		return 0
	}
	n := 0
	for _, p := range c.Parts {
		if p == nil {
			continue
		}
		switch {
		case p.Text != "":
			n += len(p.Text)
		case p.InlineData != nil:
			n += len(p.InlineData.Data)
		case p.FunctionCall != nil, p.FunctionResponse != nil:
			n += 64
		}
	}
	return n
}
