package provider

import (
	"encoding/json"

	"google.golang.org/genai"
)

// Tool-calling support across publishers.
//
// This file collects the small set of shared, publisher-agnostic helpers used
// by the per-publisher adapters when they translate inbound tool definitions
// and tool-call results to/from upstream wire formats.
//
// The internal lingua franca for tool calling is the genai SDK types:
//   - genai.Tool / genai.FunctionDeclaration: tool definitions from the caller
//   - genai.Part.FunctionCall:                model-emitted call (Name + Args)
//   - genai.Part.FunctionResponse:            client-supplied result
//   - genai.ToolConfig:                       tool_choice semantics
//
// This matches what the Google adapter (Gemini) already speaks natively via
// the SDK, and minimizes the per-adapter translation surface.

// FinishReasonToolCalls is the gateway-internal sentinel a publisher adapter
// emits on its final synthetic chunk when the upstream terminated because the
// model elected to invoke a tool. Downstream code (api/openai_stream.go's
// finishReasonOAI and api/handlers.go's doneReason) recognizes the string and
// translates it to the appropriate OpenAI ("tool_calls") / Ollama ("tool_use")
// wire value.
//
// genai's own FinishReason vocabulary doesn't include a tool-call terminal,
// so we extend it with a custom string. The type is just a string alias so
// this is safe and forward-compatible.
const FinishReasonToolCalls genai.FinishReason = "TOOL_CALLS"

// MarshalToolArgs converts a genai-style argument map into the JSON-text form
// that every wire format we target (Anthropic, OpenAI-compat, Cohere) expects
// to carry on a tool-call request envelope. Returns "{}" for nil/empty input
// so callers never have to special-case the empty case.
//
// Errors are unreachable in practice (map[string]any always marshals) but are
// surfaced for safety.
func MarshalToolArgs(args map[string]any) (string, error) {
	if len(args) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "{}", err
	}
	return string(b), nil
}

// UnmarshalToolArgs parses a JSON-text argument blob (the on-the-wire form for
// every upstream we target) into the map shape that genai.FunctionCall.Args
// uses internally. An empty/whitespace input yields a nil map without error.
//
// This is intentionally permissive: malformed JSON returns a nil map AND the
// parse error so callers can either log-and-continue (preferred for streaming
// chunks where Anthropic streams arguments byte-by-byte and intermediate
// snapshots are not valid JSON) or surface the error.
func UnmarshalToolArgs(raw string) (map[string]any, error) {
	if raw == "" {
		return nil, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, err
	}
	return args, nil
}

// FunctionResponseToText folds a genai.FunctionResponse into a single string
// that can be embedded in upstreams that don't have a structured tool-result
// slot (e.g. the legacy Ollama /api/chat path). Order of precedence:
//  1. response.output (most schemas treat this as the canonical result key)
//  2. response.error  (so error contexts aren't silently dropped)
//  3. whole response object JSON-encoded as a fallback
//
// Returns "" only when the response is genuinely empty.
func FunctionResponseToText(fr *genai.FunctionResponse) string {
	if fr == nil || len(fr.Response) == 0 {
		return ""
	}
	if out, ok := fr.Response["output"]; ok {
		if s, ok := out.(string); ok {
			return s
		}
		if b, err := json.Marshal(out); err == nil {
			return string(b)
		}
	}
	if e, ok := fr.Response["error"]; ok {
		if s, ok := e.(string); ok {
			return s
		}
		if b, err := json.Marshal(e); err == nil {
			return string(b)
		}
	}
	b, err := json.Marshal(fr.Response)
	if err != nil {
		return ""
	}
	return string(b)
}
