package api

import (
	"encoding/json"
	"time"
)

// ToolCallFunction is the inner function descriptor of an Ollama-shape
// assistant tool_call. Arguments is a JSON OBJECT (not a stringified
// JSON, unlike OpenAI's wire format) per real Ollama's convention; we keep
// it as map[string]any so json.Marshal naturally produces the nested object.
type ToolCallFunction struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// ToolCall is one tool invocation on an Ollama assistant message. Real
// Ollama serializes these without an `id` field; we omit ours similarly
// when unset to maximize client compatibility.
type ToolCall struct {
	ID       string           `json:"id,omitempty"`
	Function ToolCallFunction `json:"function"`
}

// Message represents a single message in an Ollama chat request/response.
//
// Tool-calling fields:
//   - ToolCalls is populated on assistant messages that invoked tools (sent
//     by us on Done frames, accepted from clients on replay).
//   - ToolName + ToolCallID are populated on role:"tool" messages that
//     carry a result for a prior assistant tool_call.
//
// Real Ollama uses `tool_calls` and folds tool results into either role:
// "tool" messages or back into user turns depending on the model; we accept
// both forms on input.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Images is the Ollama-native multimodal slot: a list of bare-base64
	// payloads (no `data:image/...;base64,` prefix). Each entry is
	// MIME-sniffed from the magic bytes at decode time. Only PNG, JPEG,
	// WEBP and GIF are accepted; anything else returns a 400 from the
	// /api/chat handler. See api/multimodal.go for decoding details.
	Images     []string   `json:"images,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Options represents the standard Ollama advanced parameters.
type Options struct {
	Temperature float32  `json:"temperature,omitempty"`
	TopP        float32  `json:"top_p,omitempty"`
	TopK        int32    `json:"top_k,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	NumPredict  int32    `json:"num_predict,omitempty"`
}

// ToolFunctionDef describes a callable function in an Ollama-shape `tools`
// request slot. Parameters is JSON Schema, kept as RawMessage so we can
// pass it through to every publisher adapter without re-shaping.
type ToolFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolDef is one entry in the Ollama /api/chat `tools` request array. Type
// is always "function" — matches real Ollama's shape exactly (which itself
// mirrors OpenAI's).
type ToolDef struct {
	Type     string          `json:"type"` // "function"
	Function ToolFunctionDef `json:"function"`
}

// ChatRequest represents the payload for the Ollama /api/chat endpoint.
//
// Tools and Format are real Ollama fields:
//   - Tools is the function-calling tool list (translated to publisher-
//     native shapes downstream).
//   - Format is accepted for decode compatibility but not actively wired
//     to publisher response_format settings — present so clients that
//     always send `format:"json"` don't fail to parse.
type ChatRequest struct {
	Model    string          `json:"model"`
	Messages []Message       `json:"messages"`
	Stream   *bool           `json:"stream,omitempty"` // Default is true in Ollama
	Options  *Options        `json:"options,omitempty"`
	Tools    []ToolDef       `json:"tools,omitempty"`
	Format   json.RawMessage `json:"format,omitempty"` // accepted, ignored
}

// ChatResponse represents a chunk or final response from Ollama /api/chat endpoint.
//
// The duration fields are reported in nanoseconds (matching the real Ollama
// daemon) and consumed by clients like Cline to render timing/tokens-per-second
// statistics. They are only meaningful in the final Done=true message but are
// always emitted so JSON shape is stable.
type ChatResponse struct {
	Model              string    `json:"model"`
	CreatedAt          time.Time `json:"created_at"`
	Message            Message   `json:"message"`
	Done               bool      `json:"done"`
	DoneReason         string    `json:"done_reason,omitempty"`
	TotalDuration      int64     `json:"total_duration,omitempty"`
	LoadDuration       int64     `json:"load_duration,omitempty"`
	PromptEvalCount    int32     `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64     `json:"prompt_eval_duration,omitempty"`
	EvalCount          int32     `json:"eval_count,omitempty"`
	EvalDuration       int64     `json:"eval_duration,omitempty"`
}

// GenerateRequest represents the payload for the Ollama /api/generate endpoint.
type GenerateRequest struct {
	Model   string   `json:"model"`
	Prompt  string   `json:"prompt"`
	System  string   `json:"system,omitempty"`
	Stream  *bool    `json:"stream,omitempty"`
	Options *Options `json:"options,omitempty"`
}

// GenerateResponse represents a chunk or final response from Ollama /api/generate endpoint.
type GenerateResponse struct {
	Model              string    `json:"model"`
	CreatedAt          time.Time `json:"created_at"`
	Response           string    `json:"response"`
	Done               bool      `json:"done"`
	DoneReason         string    `json:"done_reason,omitempty"`
	TotalDuration      int64     `json:"total_duration,omitempty"`
	LoadDuration       int64     `json:"load_duration,omitempty"`
	PromptEvalCount    int32     `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64     `json:"prompt_eval_duration,omitempty"`
	EvalCount          int32     `json:"eval_count,omitempty"`
	EvalDuration       int64     `json:"eval_duration,omitempty"`
}

// ModelDetails mimics the structure Ollama returns for each tag entry so
// clients like Cline can render family/parameter-size badges.
type ModelDetails struct {
	ParentModel       string   `json:"parent_model,omitempty"`
	Format            string   `json:"format,omitempty"`
	Family            string   `json:"family,omitempty"`
	Families          []string `json:"families,omitempty"`
	ParameterSize     string   `json:"parameter_size,omitempty"`
	QuantizationLevel string   `json:"quantization_level,omitempty"`
}

// TagModel is the per-model entry returned from /api/tags.
type TagModel struct {
	Name       string       `json:"name"`
	Model      string       `json:"model"`
	ModifiedAt time.Time    `json:"modified_at"`
	Size       int64        `json:"size"`
	Digest     string       `json:"digest,omitempty"`
	Details    ModelDetails `json:"details"`
}

// TagsResponse is the wire format for /api/tags.
type TagsResponse struct {
	Models []TagModel `json:"models"`
}
