package provider

import "google.golang.org/genai"

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
func applyCompressionPipeline(contents []*genai.Content, systemPrompt string) ([]*genai.Content, string) {
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

	return contents, systemPrompt
}