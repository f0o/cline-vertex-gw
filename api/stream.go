package api

import "google.golang.org/genai"

// StreamDelta is the publisher-agnostic chunk type emitted by
// runStreamWithRetry's per-chunk callback. Exactly one of the payload
// "shape" fields should be populated per delta:
//
//   - Text != ""               → a text fragment from the model
//   - FunctionCall != nil      → a complete tool/function call (Anthropic and
//     Cohere emit one chunk per call; OpenAI-compat
//     accumulates streamed args internally and
//     emits one chunk per call after content_block_
//     stop / final delta)
//
// Surface emitters (Ollama, OpenAI) translate this to their respective
// streaming wire shapes. Surface emitters that don't support tool calls on
// their wire (currently the Ollama /api/generate surface) simply ignore
// FunctionCall deltas and only forward Text fragments — losing visibility
// into the model's tool-call decisions but never crashing.
type StreamDelta struct {
	Text string
	Part *genai.Part
}

// streamCallback is invoked for each delta produced by the streaming
// upstream call. Returning an error from the callback aborts streaming.
type streamCallback func(d StreamDelta) error
