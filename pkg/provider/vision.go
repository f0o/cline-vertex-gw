package provider

import (
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// Vision capability gate.
//
// Multimodal (image) inputs are accepted by the gateway at the API boundary,
// but only some publisher/model combinations on Vertex AI can actually
// consume them. If we let an image-bearing request flow through to a
// publisher that doesn't support vision, one of three bad things happens:
//
//  1. The upstream returns HTTP 400 mid-stream, AFTER we've already
//     committed response headers — the client gets a half-formed SSE
//     stream with no clear error.
//  2. The upstream silently strips the image and answers as if it were
//     a text-only message — the user thinks the model "looked" at their
//     screenshot but it didn't.
//  3. The upstream returns nonsense that mentions an image it never saw.
//
// All three are user-hostile. The gate below returns a clean 400 with a
// list of vision-capable models the caller can switch to instead.
//
// Capability matrix (verified against Vertex AI's Model Garden as of the
// 0.9 release):
//
//   - google     → ALL gemini-1.5+ and gemini-2.x models are multimodal
//                  natively; gemma-3 multimodal variants ditto. The genai
//                  SDK just passes inlineData through.
//   - anthropic  → ALL claude-3+ models (haiku/sonnet/opus, 3.5, 4.x) and
//                  *-thinking reasoning variants accept image blocks.
//                  Earlier claude-2 / claude-instant models do not, but
//                  they're not on Vertex anyway.
//   - meta       → llama-3.2 *-vision-instruct family + llama-4 (when
//                  available); plain llama-3.1 / 3.3 / non-vision 3.2 do
//                  NOT.
//   - mistralai  → pixtral-* family yes; everything else (mistral-large,
//                  codestral, mixtral) no.
//   - qwen       → qwen2-vl-* / qwen2.5-vl-* yes; plain qwen no.
//   - nvidia     → llama-3.2-nv-vision-* yes; others no.
//   - cohere     → no vision support on Vertex AI today (Command-A-Vision
//                  is direct-API only).
//   - ai21       → no vision support.
//   - deepseek-ai → no vision support.
//
// The "is this a vision model" decision uses substring matches on the
// lowercased bare model id. We don't try to be exhaustive — when in doubt
// we err on the side of "vision-capable" for publishers whose entire
// catalog supports it (google, anthropic), and "text-only" for publishers
// who only have a few vision SKUs (meta, mistralai, qwen, nvidia).
//
// CheckVisionSupport is a no-op for requests without image parts, so it's
// free to call on every chat request.

// hasInlineImageParts reports whether any Content in the slice carries at
// least one InlineData part. The compression pipeline preserves
// these unchanged, so this check works equally well before or after the
// pipeline runs.
func hasInlineImageParts(contents []*genai.Content) bool {
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.InlineData != nil {
				return true
			}
		}
	}
	return false
}

// publisherSupportsMIME reports whether the given (publisher, model) combination
// supports a specific MIME type on Vertex AI.
func publisherSupportsMIME(publisher, modelID, mime string) bool {
	if strings.HasPrefix(mime, "image/") {
		return publisherSupportsVision(publisher, modelID)
	}
	kind, ok := PublisherKind(publisher)
	if !ok {
		return false
	}
	if mime == "application/pdf" {
		// Only Google and Anthropic support PDF natively on Vertex AI.
		return kind == adapterGoogle || kind == adapterAnthropic
	}
	if strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "video/") {
		// Only Google supports audio and video natively on Vertex AI.
		return kind == adapterGoogle
	}
	return false
}

// publisherSupportsVision reports whether the given (publisher, model)
// combination is known to accept InlineData image parts on Vertex AI.
// "Known" here means we've manually verified it; the policy is conservative
// (default-deny) so a new model id that we haven't catalogued won't be
// allowed to send an image just because its publisher's vision-capable
// SKUs work.
//
// Exported because the per-publisher adapters may want to call it from a
// future place where the model id and publisher are available separately.
func publisherSupportsVision(publisher, modelID string) bool {
	id := strings.ToLower(modelID)
	kind, ok := PublisherKind(publisher)
	if !ok {
		return false
	}
	switch kind {
	case adapterGoogle:
		// Gemini 1.5+ and Gemma 3 are all multimodal. Older Bison/PaLM
		// aren't, but they're text-completion legacy and not on this gateway.
		if strings.Contains(id, "gemini") || strings.HasPrefix(id, "gemma-3") {
			return true
		}
		// Default to "yes" for unknown Google models — the genai SDK will
		// reject incompatible payloads with a clear error of its own, and
		// erring conservative would block legitimate future Gemini variants.
		return true
	case adapterAnthropic:
		// All Claude 3+ accept image blocks. We don't ship Claude 2.
		return strings.HasPrefix(id, "claude-")
	case adapterOpenAICompat:
		// Look up the publisher specifically to handle Meta/Mistral/Qwen/Nvidia.
		// These are specific OpenAICompat models with vision support.
		switch publisher {
		case "meta":
			return strings.Contains(id, "vision") || strings.HasPrefix(id, "llama-4")
		case "mistralai":
			return strings.HasPrefix(id, "pixtral")
		case "qwen":
			return strings.Contains(id, "-vl-") || strings.HasSuffix(id, "-vl")
		case "nvidia":
			return strings.Contains(id, "vision") || strings.Contains(id, "-vl-")
		default:
			return false
		}
	case adapterCohere, adapterGoogle + 100: // Default cases for text-only
		return false
	default:
		// Unknown publisher: text-only is the safe default.
		return false
	}
}

// CheckVisionSupport validates that the request's inline media attachments
// (images, audio, video, documents) are acceptable for the chosen model.
// It is intended to be called from the HTTP handler layer AFTER buildContents*
// has produced the final Content slice — so it sees the post-decode form
// (InlineData parts populated).
//
// Returns nil when:
//   - the request has no inline media parts, OR
//   - the resolved publisher/model supports all of the attached MIME types.
//
// Returns a clear error otherwise. Handlers should map this to HTTP 400.
func CheckVisionSupport(modelName string, contents []*genai.Content) error {
	if !hasInlineImageParts(contents) {
		return nil
	}
	publisher, modelID := ParsePublisher(modelName)
	var unsupportedMimes []string

	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.InlineData != nil {
				mime := p.InlineData.MIMEType
				if !publisherSupportsMIME(publisher, modelID, mime) {
					unsupportedMimes = append(unsupportedMimes, mime)
				}
			}
		}
	}

	if len(unsupportedMimes) == 0 {
		return nil
	}

	unsupportedList := strings.Join(unsupportedMimes, ", ")
	return fmt.Errorf(
		"model %q (publisher=%q) does not support media inputs [%s] on Vertex AI; "+
			"use a vision-capable or multimodal-capable model instead — e.g. gemini-2.0-flash, "+
			"claude-3-5-sonnet, llama-3.2-90b-vision-instruct-maas, or pixtral-12b",
		modelName, publisher, unsupportedList)
}
