package provider

import "google.golang.org/genai"

// Configuration to automatically inject an XML compliance reminder/hint
// for Google/Gemini models to help them format tool tags correctly.
// Enabled by default; set GW_GEMINI_XML_HINT=off to disable.
var geminiXmlHint = envBool("GW_GEMINI_XML_HINT", true)

// geminiXMLHintText is the strict XML-format reminder appended to Gemini system
// prompts so the model emits raw (unescaped) tool tags. Kept as a package const
// rather than an inline literal for readability and easy tuning.
const geminiXMLHintText = "\n\n" +
	"IMPORTANT FOR TOOL CALLS:\n" +
	"When using tools, you MUST output the XML tags and parameters exactly as specified in raw form. " +
	"Under no circumstances should you HTML-escape characters: do NOT output '<' or '>' — always use raw '<' and '>'. " +
	"Do NOT wrap XML tool blocks or tags in markdown code blocks (such as ```xml or ```). " +
	"Ensure every XML opening tag is correctly matched with its closing tag."

// applyCompressionPipeline runs the gateway's in-flight prompt compression
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
func applyCompressionPipeline(contents []*genai.Content, systemPrompt string, modelName string) ([]*genai.Content, string) {
	// 0. Resolve any LLM loop-trap by deduplicating scoldings and empty turns first.
	contents = BreakLoopTrap(contents)

	// 0b. Prune superseded read-only tool exchanges (e.g. an old read_file
	//     whose target the model has since re-read). Runs early, before the
	//     byte-shaping steps, so later stages operate on the smaller history.
	//     Opt-in (GW_PRUNE_STALE_TOOLS, default off) — it removes whole turns,
	//     so it ships disabled and is gated by strict call/response-pair safety.
	contents = PruneStaleTools(contents)

	// 1. Normalize whitespace FIRST. CRLF/CR collapse, trailing-space
	//    trim, and blank-line capping shrink each block in a lossless
	//    way. Doing this before the byte-budget trim means the trim
	//    operates on already-cleaned sizes, so the budget goes further.
	//    Doing it before the env-block scanner means the tag-matching
	//    regex doesn't have to deal with CRLF-ified <environment_details>
	//    markers (which we haven't observed, but defensively cover).
	systemPrompt = NormalizeSystemPrompt(systemPrompt)
	contents = NormalizeWhitespace(contents)

	// 2. Collapse stale <environment_details> blocks on every user turn
	//    except the most recent. This is a Cline-specific transform; it's
	//    a no-op when no such tags appear (so it's harmless for
	//    non-Cline OpenAI-API callers like LiteLLM / openai-python).
	//    Must run BEFORE TrimContents so the trim budget is computed
	//    against post-collapse sizes.
	contents = CollapseEnvBlocks(contents)

	// 2b. Middle-elide oversized tool results (read_file dumps, terminal
	//     output) on every turn except the latest. Runs BEFORE TrimContents
	//     so the byte budget is computed against the shrunken sizes (and may
	//     avoid dropping whole turns), and BEFORE Dedup so dedup hashes the
	//     post-truncation bodies. On by default (GW_TOOL_RESULT_TRUNCATE) —
	//     low-risk because it keeps head+tail and never touches the latest turn.
	contents = TruncateToolResults(contents)

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

	// Resolve the publisher ONCE and reuse it for the Gemini-only steps 5 & 6.
	pub, _ := ParsePublisher(modelName)

	// 5. Inject Gemini XML compliance instructions if the target is a Gemini model.
	if pub == "google" {
		systemPrompt = appendGeminiXMLHint(systemPrompt)
	}

	// 6. Align function calls and responses for Gemini/Vertex AI.
	if pub == "google" {
		contents = AlignFunctionCallsAndResponses(contents)
	}

	return contents, systemPrompt
}

// InjectGeminiXMLHint appends a strict XML format compliance directive to the
// system prompt for Google/Gemini models, forcing them to emit unescaped tags
// directly. It is a no-op for non-Google models or when the hint is disabled.
func InjectGeminiXMLHint(systemPrompt string, modelName string) string {
	if pub, _ := ParsePublisher(modelName); pub != "google" {
		return systemPrompt
	}
	return appendGeminiXMLHint(systemPrompt)
}

// appendGeminiXMLHint appends geminiXMLHintText to systemPrompt, honoring the
// GW_GEMINI_XML_HINT knob and skipping empty prompts. The caller is responsible
// for confirming the target is a Gemini model.
func appendGeminiXMLHint(systemPrompt string) string {
	if !geminiXmlHint || systemPrompt == "" {
		return systemPrompt
	}
	return systemPrompt + geminiXMLHintText
}
