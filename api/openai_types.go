package api

// OpenAI-compatible API types.
//
// This file defines the wire shapes for the subset of the OpenAI Chat
// Completions and Models APIs that this gateway implements. The goal is
// drop-in compatibility with clients that speak OpenAI Chat Completions
// (LiteLLM, LangChain, Continue, Cline's "OpenAI Compatible" provider,
// the official openai-python / openai-node SDKs, etc.).
//
// Tool / function calling is fully supported on both surfaces:
//   - Inbound `tools`         → translated to publisher-specific shapes
//   - Inbound `tool_choice`   → "auto" | "none" | "required" | {function:{name}}
//   - Inbound `tool_calls`    → assistant turn carrying prior tool invocations
//   - Inbound role:"tool"     → tool-result reply (folded into the conversation)
//   - Outbound `tool_calls`   → emitted on both stream and non-stream paths
//   - Outbound finish_reason  → "tool_calls" when the model invoked a tool
//
// Multimodal content (images/audio) is not supported here; only the string
// form of `content` is implemented. The struct uses a json.RawMessage to
// tolerate either string or array bodies on the wire and then decodes to
// string. Non-string content is flattened to its concatenated text.
//
// Other accepted-but-ignored fields:
//   - `n > 1`, `logprobs`, `response_format`, `seed`, `user`
//   - `frequency_penalty`, `presence_penalty`, `logit_bias`
//   - legacy `functions` / `function_call` (deprecated since 2023; modern
//     clients should use `tools` / `tool_choice` which we DO support)
//
// Reference: https://platform.openai.com/docs/api-reference/chat
//            https://platform.openai.com/docs/api-reference/models

import (
	"encoding/json"
	"strings"
)

// OAIToolFunctionDef describes a callable function for the OpenAI `tools`
// request slot. Parameters is a JSON-Schema object describing the function's
// arguments; we keep it as RawMessage so we can pass it through to every
// publisher adapter without re-shaping (each upstream parses it natively).
type OAIToolFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// OAIToolDef is one entry in the OpenAI `tools` request array. Type is
// always "function" per the OpenAI spec.
type OAIToolDef struct {
	Type     string             `json:"type"` // "function"
	Function OAIToolFunctionDef `json:"function"`
}

// OAIToolChoice is OpenAI's polymorphic tool_choice field:
//   - "auto"     — model may answer or call a tool (default)
//   - "none"     — model MUST NOT call a tool
//   - "required" — model MUST call SOME tool
//   - {type:"function", function:{name:"<fn>"}} — pin to a specific function
//
// We accept all four shapes via custom UnmarshalJSON. MarshalJSON is not
// implemented because we never re-serialize the original wire form — the
// internal representation is consumed by the provider layer.
type OAIToolChoice struct {
	// Mode is the canonical bare-string form: "auto" | "none" | "required" |
	// "function". When Mode == "function", Name carries the pinned function
	// name. Mode == "" means the field was absent (callers should treat as
	// "auto").
	Mode string
	Name string
}

// UnmarshalJSON accepts either of:
//
//	"auto" | "none" | "required"
//	{"type":"function","function":{"name":"foo"}}
//
// Any other shape decodes to {Mode:"auto"} as a defensive default rather
// than failing the request — OpenAI clients have been known to ship oddly-
// shaped values here and the safest fallback is "let the model decide".
func (c *OAIToolChoice) UnmarshalJSON(data []byte) error {
	// Try the bare-string form first.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.Mode = strings.ToLower(strings.TrimSpace(s))
		if c.Mode == "" {
			c.Mode = "auto"
		}
		return nil
	}
	// Object form.
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(data, &obj); err == nil && obj.Type == "function" && obj.Function.Name != "" {
		c.Mode = "function"
		c.Name = obj.Function.Name
		return nil
	}
	// Fallback.
	c.Mode = "auto"
	return nil
}

// OAIToolCall is one assistant tool_call as it appears on the wire — both
// inbound (when a client replays an assistant turn that previously invoked a
// tool) and outbound (in our response when the model emits a tool call).
//
// Per the OpenAI spec, Function.Arguments is a JSON-encoded STRING (NOT a
// JSON object). Many clients trip over this — we always emit it as a string.
type OAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// OAIChatMessage is a single OpenAI-shaped chat message.
//
// `Content` is declared as json.RawMessage so we can accept either of:
//   - "content": "hello"
//   - "content": [{"type":"text","text":"hello"}, ...]
//
// The decode helper ContentString() flattens both forms to a plain string.
// `ToolCalls` is populated on assistant turns that previously invoked tools;
// `ToolCallID` is populated on role:"tool" turns to link the result to the
// originating call.
type OAIChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []OAIToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	// FunctionCall is the legacy pre-tools field accepted for decode
	// compatibility with older clients but ignored downstream.
	FunctionCall json.RawMessage `json:"function_call,omitempty"`
}

// ContentString flattens the message content to a plain string regardless of
// whether the client sent a bare string or a content-parts array. Unknown
// part types are skipped. Returns the empty string if content is missing.
func (m OAIChatMessage) ContentString() string {
	if len(m.Content) == 0 {
		return ""
	}
	// Try the simple string form first.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	// Fall back to the parts-array form.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var out string
		for _, p := range parts {
			if p.Type == "" || p.Type == "text" {
				out += p.Text
			}
		}
		return out
	}
	return ""
}

// OAIChatRequest is the POST /v1/chat/completions body.
type OAIChatRequest struct {
	Model       string           `json:"model"`
	Messages    []OAIChatMessage `json:"messages"`
	Stream      bool             `json:"stream,omitempty"`
	Temperature *float32         `json:"temperature,omitempty"`
	TopP        *float32         `json:"top_p,omitempty"`
	// TopK is non-standard for OpenAI but some compatible servers accept it.
	TopK             *int32   `json:"top_k,omitempty"`
	MaxTokens        *int32   `json:"max_tokens,omitempty"`
	MaxOutputTokens  *int32   `json:"max_output_tokens,omitempty"` // OpenAI o-series alias
	Stop             []string `json:"stop,omitempty"`
	N                int      `json:"n,omitempty"`                 // accepted, ignored (n>1 not supported)
	Seed             *int     `json:"seed,omitempty"`              // accepted, ignored
	User             string   `json:"user,omitempty"`              // accepted, ignored
	PresencePenalty  *float32 `json:"presence_penalty,omitempty"`  // accepted, ignored
	FrequencyPenalty *float32 `json:"frequency_penalty,omitempty"` // accepted, ignored
	// Tool / function calling — fully supported.
	Tools      []OAIToolDef   `json:"tools,omitempty"`
	ToolChoice *OAIToolChoice `json:"tool_choice,omitempty"`
	// Legacy pre-tools fields. Accepted to avoid decode errors; we silently
	// promote `functions` to `tools` so older clients still get tool support.
	Functions    []OAIToolFunctionDef `json:"functions,omitempty"`
	FunctionCall json.RawMessage      `json:"function_call,omitempty"`
	// Accepted to avoid decode errors but ignored downstream.
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	Logprobs       *bool           `json:"logprobs,omitempty"`
	TopLogprobs    *int            `json:"top_logprobs,omitempty"`
}

// OAIResponseMessage is the assistant message returned in a chat completion.
// ToolCalls is populated when the model invoked one or more tools.
type OAIResponseMessage struct {
	Role      string        `json:"role"`
	Content   string        `json:"content"`
	ToolCalls []OAIToolCall `json:"tool_calls,omitempty"`
}

// OAIChatChoice is a single completion choice (we only ever return one).
type OAIChatChoice struct {
	Index        int                 `json:"index"`
	Message      OAIResponseMessage  `json:"message,omitempty"`
	Delta        *OAIResponseMessage `json:"delta,omitempty"` // only used in streaming
	FinishReason string              `json:"finish_reason,omitempty"`
}

// OAIStreamToolCallDelta is one entry in a streaming `delta.tool_calls`
// array. The first delta for a given Index carries ID+Type+Function.Name;
// subsequent deltas append to Function.Arguments. This matches OpenAI's
// reference wire format exactly.
type OAIStreamToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"` // "function"
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

// OAIStreamDelta is the per-chunk delta in a streaming response. Either
// Content OR ToolCalls (but not typically both in the same chunk) carries
// the per-chunk payload; Role is set only on the very first delta.
type OAIStreamDelta struct {
	Role      string                   `json:"role,omitempty"`
	Content   string                   `json:"content,omitempty"`
	ToolCalls []OAIStreamToolCallDelta `json:"tool_calls,omitempty"`
}

// OAIChatChoiceStream is a single completion delta choice for streaming.
type OAIChatChoiceStream struct {
	Index        int            `json:"index"`
	Delta        OAIStreamDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

// OAIUsage matches OpenAI's usage block. Fields are omitted when zero so
// non-streaming responses don't carry empty usage in the streaming chunks.
type OAIUsage struct {
	PromptTokens     int32 `json:"prompt_tokens"`
	CompletionTokens int32 `json:"completion_tokens"`
	TotalTokens      int32 `json:"total_tokens"`
}

// OAIChatResponse is the non-streaming response body for /v1/chat/completions.
type OAIChatResponse struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`  // "chat.completion"
	Created           int64           `json:"created"` // unix seconds
	Model             string          `json:"model"`
	SystemFingerprint string          `json:"system_fingerprint,omitempty"`
	Choices           []OAIChatChoice `json:"choices"`
	Usage             OAIUsage        `json:"usage"`
}

// OAIChatStreamResponse is a single SSE chunk for /v1/chat/completions
// streams. Object is "chat.completion.chunk". Usage is included only on the
// final chunk (matching the OpenAI behavior when stream_options.include_usage
// is true; we always include it on the final chunk for simplicity and
// broader client compatibility).
type OAIChatStreamResponse struct {
	ID                string                `json:"id"`
	Object            string                `json:"object"` // "chat.completion.chunk"
	Created           int64                 `json:"created"`
	Model             string                `json:"model"`
	SystemFingerprint string                `json:"system_fingerprint,omitempty"`
	Choices           []OAIChatChoiceStream `json:"choices"`
	Usage             *OAIUsage             `json:"usage,omitempty"`
}

// OAIModel is a single entry in GET /v1/models.
type OAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`  // always "model"
	Created int64  `json:"created"` // unix seconds; we use current time
	OwnedBy string `json:"owned_by"`
}

// OAIModelsResponse is the GET /v1/models response body.
type OAIModelsResponse struct {
	Object string     `json:"object"` // always "list"
	Data   []OAIModel `json:"data"`
}

// OAIErrorResponse is the standard OpenAI error envelope.
type OAIErrorResponse struct {
	Error OAIErrorBody `json:"error"`
}

// OAIErrorBody is the inner object of an OpenAI error response.
type OAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}
