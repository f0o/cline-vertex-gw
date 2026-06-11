package pipeline

import (
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"strings"

	"google.golang.org/genai"
)

// GenerationOptions is the representation of generation tuning
// options passed from the client and processed by both HTTP handlers
// and the prompt-shaping compression pipeline.
type GenerationOptions struct {
	Temperature *float32
	TopP        *float32
	TopK        *int32
	Stop        []string
	MaxTokens   *int32
	Tools       []*genai.Tool
	ToolConfig  *genai.ToolConfig
}

// onCompressionSaved is wired by the api package via the SetCompressionMetrics
// hook so this package stays metrics-agnostic (avoids an import cycle back
// into the api package).
var onCompressionSaved = func(stage string, bytes int) {}

// SetCompressionMetrics installs the callback for recording bytes saved.
func SetCompressionMetrics(cb func(stage string, bytes int)) {
	if cb != nil {
		onCompressionSaved = cb
	}
}

// LogOptimizerPipelineConfiguration prints a single INFO-level log showing all prompt optimization
// knobs and parameters that are currently active or loaded.
func LogOptimizerPipelineConfiguration() {
	var active []string
	if breakLoopTrapEnabled {
		active = append(active, fmt.Sprintf("loopbreak(nudge=%t)", loopTrapNudgeEnabled))
	}
	if pruneStaleTools {
		active = append(active, "prune_tools")
	}
	if collapseEnvBlocks {
		active = append(active, fmt.Sprintf("envblocks(min_bytes=%d)", collapseEnvMinBytes))
	}
	if toolResultTruncate {
		active = append(active, fmt.Sprintf("toolresult(max_bytes=%d,head=%d,tail=%d)", toolResultMaxBytes, toolResultHeadBytes, toolResultTailBytes))
	}
	if deepCompactEnabled {
		active = append(active, fmt.Sprintf("deepcompact(keep_turns=%d,max_bytes=%d,head=%d,tail=%d)", deepCompactKeepTurns, deepCompactMaxBytes, deepCompactHeadBytes, deepCompactTailBytes))
	}
	if activeToolPruningEnabled {
		active = append(active, fmt.Sprintf("active_tool_pruning(window=%d)", activeToolPruningWindow))
	}
	if maxInputChars > 0 {
		active = append(active, fmt.Sprintf("trim(max_chars=%d)", maxInputChars))
	}
	if dedupReplay {
		active = append(active, fmt.Sprintf("dedup(min_bytes=%d)", dedupMinBytes))
	}
	if dedupSubstring {
		active = append(active, fmt.Sprintf("dedup_substring(min_bytes=%d)", dedupSubstringMinBytes))
	}
	if cacheAlignerEnabled {
		active = append(active, "cache_aligner")
	}

	logx.For("optimizer").Info("prompt optimizer pipeline loaded",
		"active_optimizers", strings.Join(active, ", "),
	)
}

// ApplyCompressionPipeline runs the gateway's in-flight prompt compression
// stack against the conversation about to be sent upstream. The exact order
// matters and is documented inline below; do NOT reshuffle without reading
// the rationale.
//
// All steps are individually controllable via GW_* env knobs; with every
// knob off this is a near-zero-cost pass-through (each helper has a
// disabled-fast-path that returns the input slice header unchanged).
//
// Returns the (possibly rewritten) contents and (possibly normalized)
// system prompt to forward to the per-publisher adapter.
func ApplyCompressionPipeline(contents []*genai.Content, systemPrompt string, isGemini bool, opts *GenerationOptions) ([]*genai.Content, string) {
	// 0. Resolve any LLM loop-trap by deduplicating scoldings and empty turns first.
	contents = BreakLoopTrap(contents)

	// 0a. Dynamic active tool pruning based on sliding-window usage
	PruneActiveTools(contents, opts)

	// 0b. Prune superseded read-only tool exchanges (e.g. an old read_file
	//     whose target the model has since re-read). Runs early, before the
	//     byte-shaping steps, so later stages operate on the smaller history.
	//     Opt-in (GW_PRUNE_STALE_TOOLS, default off) — it removes whole turns,
	//     so it ships disabled and is gated by strict call/response-pair safety.
	if !HasRetrievalToolCall(contents) {
		contents = PruneStaleTools(contents)
	}

	// 1. Normalize whitespace FIRST. CRLF/CR collapse, trailing-space
	//    trim, and blank-line capping shrink each block in a lossless
	//    way. Doing this before the byte-budget trim means the trim
	//    operates on already-cleaned sizes, so the budget goes further.
	//    Doing it before the env-block scanner means the tag-matching
	//    regex doesn't have to deal with CRLF-ified <environment_details>
	//    markers (which we haven't observed, but defensively cover).
	systemPrompt = NormalizeSystemPrompt(systemPrompt)
	systemPrompt = AlignSystemPromptCache(systemPrompt)
	contents = NormalizeWhitespace(contents)

	// 2. Collapse stale <environment_details> blocks on every user turn
	//    except the most recent. This is a Cline-specific transform; it's
	//    a no-op when no such tags appear (so it's harmless for
	//    non-Cline OpenAI-API callers like LiteLLM / openai-python).
	//    Must run BEFORE TrimContents so the trim budget is computed
	//    against post-collapse sizes.
	contents = CollapseEnvBlocks(contents)

	if !HasRetrievalToolCall(contents) {
		// 2b. Middle-elide oversized tool results (read_file dumps, terminal
		//     output) on every turn except the latest. Runs BEFORE TrimContents
		//     so the byte budget is computed against the shrunken sizes (and may
		//     avoid dropping whole turns), and BEFORE Dedup so dedup hashes the
		//     post-truncation bodies. On by default (GW_TOOL_RESULT_TRUNCATE) —
		//     low-risk because it keeps head+tail and never touches the latest turn.
		contents = TruncateToolResults(contents)

		// 2b1. Elide massive historical file write and modification tool calls (write_to_file, replace_in_file)
		//      on assistant turns older than 2 turns. This is completely lossless as full contents are
		//      saved in FSCache and retrievable on-demand via retrieve_elided_content.
		contents = ElideHistoricalWriteActions(contents)

		// 2c. Deeply compact cold historical turns (turns older than GW_DEEP_COMPACT_KEEP_TURNS)
		//     by aggressively shrinking any large text blocks or tool outputs down to tiny
		//     high-density placeholders. Runs BEFORE TrimContents so that shrunk turns fit
		//     into the byte budget and are preserved in context instead of being discarded entirely.
		contents = DeepCompactHistoricalTurns(contents)
	}

	// 3. Drop oldest non-system turns until the conversation fits the
	//    GW_MAX_INPUT_CHARS byte budget. Must run BEFORE the per-adapter
	//    caching tagging — trimming after would orphan cache_control
	//    markers on dropped blocks. Also must run BEFORE Dedup so dedup
	//    only operates on the turns we're actually shipping.
	contents = TrimContents(contents, systemPrompt)

	// 4. Replace verbatim re-pastes of large blocks (≥ GW_DEDUP_MIN_BYTES)
	//    with a back-pointing placeholder. Must run AFTER trim (so we
	//    don't waste cycles hashing about-to-be-dropped content) and
	//    BEFORE the per-adapter caching tagging (because the replaced
	//    block is now a different prefix than what an earlier turn
	//    cached, so we want the cache markers placed against the
	//    post-dedup body).
	contents = DedupReplayedBlocks(contents)

	// 4b. Collapse PARTIAL re-pastes: an earlier large block embedded verbatim
	//     inside a later same-role turn. Runs AFTER exact dedup (which is a
	//     strict subset and cheaper). Opt-in (GW_DEDUP_SUBSTRING, default off)
	//     because it is more aggressive than whole-block dedup.
	contents = DedupSubstringBlocks(contents)

	// 5. Align function calls and responses for Gemini/Vertex AI.
	if isGemini {
		contents = AlignFunctionCallsAndResponses(contents)
	}

	// 6. Inject the dynamic retrieve_elided_content tool if any elided hashes exist in context.
	InjectRetrieveElidedContentTool(contents, opts)

	return contents, systemPrompt
}
