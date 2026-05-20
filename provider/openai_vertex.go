package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"

	"google.golang.org/genai"
)

// Llama (meta), Mistral / Mixtral / Codestral (mistralai), Jamba (ai21),
// DeepSeek, Qwen and several Nvidia-hosted models are all served on Vertex AI
// via `:rawPredict` / `:streamRawPredict` using the OpenAI-compatible
// chat-completions body shape ("model"/"messages"/"max_tokens"/"stream"...).
//
// This file implements a single shared adapter for that family. The endpoint
// is the same for every publisher; only the URL path's "publishers/<name>"
// segment varies. Streaming uses OpenAI's SSE convention with a final
// `data: [DONE]` sentinel.

// openaiToolFunction is the inner descriptor of a tool definition.
type openaiToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// openaiToolDef is one entry in the request's `tools` array. Type is
// currently always "function" — OpenAI reserves the slot for future tool
// kinds but the OpenAI-compatible Vertex MaaS publishers only support
// function tools.
type openaiToolDef struct {
	Type     string             `json:"type"` // always "function"
	Function openaiToolFunction `json:"function"`
}

// openaiToolChoice marshals to one of:
//   - "auto" / "none" / "required"  (bare string)
//   - {"type":"function","function":{"name":"<fn>"}}
//
// It supports either form via a custom MarshalJSON. The internal
// representation uses Mode+Name; callers don't touch the wire shape.
type openaiToolChoice struct {
	Mode string // "auto" | "none" | "required" | "function"
	Name string // only used when Mode == "function"
}

func (c openaiToolChoice) MarshalJSON() ([]byte, error) {
	if c.Mode == "function" && c.Name != "" {
		return json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]any{"name": c.Name},
		})
	}
	if c.Mode == "" {
		return json.Marshal("auto")
	}
	return json.Marshal(c.Mode)
}

// openaiAssistantToolCall is an assistant-message tool_call (sent on prior
// assistant turns in a multi-turn tool conversation, and received in
// response chunks). Arguments is a JSON-encoded string per the OpenAI spec
// (NOT a JSON object — this trips up many implementations).
type openaiAssistantToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openaiContentPart is one entry in the polymorphic content-parts array
// used by vision-capable OpenAI-compatible MaaS publishers (Llama 3.2
// Vision, Pixtral, Qwen2-VL, …). Each part is either text or an image
// reference via a data: URL — exactly the wire shape the original OpenAI
// Chat Completions API documents.
type openaiContentPart struct {
	Type     string             `json:"type"` // "text" | "image_url"
	Text     string             `json:"text,omitempty"`
	ImageURL *openaiImageURLObj `json:"image_url,omitempty"`
}

// openaiImageURLObj nests the image URL under image_url.url, matching
// OpenAI's spec. We always ship the bytes inline as a base64 data: URL —
// remote URL fetching is out of scope for the gateway (see api/multimodal.go
// for the SSRF rationale).
type openaiImageURLObj struct {
	URL string `json:"url"`
}

// openaiMessage is one entry in the chat-completions Messages array.
// Content may be empty when ToolCalls is populated (assistant turn that
// only emits tool calls). Role is one of "system" | "user" | "assistant" |
// "tool".
//
// Polymorphic Content: OpenAI accepts either a bare string OR a parts
// array. We use bare string for the text-only fast path (cheaper to
// serialize) and the parts array only when the turn carries images. The
// MarshalJSON below selects the right wire form based on which field is
// populated; on the wire we never emit both.
type openaiMessage struct {
	Role         string                    `json:"role"`
	Content      string                    `json:"-"`
	ContentParts []openaiContentPart       `json:"-"`
	Name         string                    `json:"name,omitempty"`
	ToolCallID   string                    `json:"tool_call_id,omitempty"`
	ToolCalls    []openaiAssistantToolCall `json:"tool_calls,omitempty"`
}

// MarshalJSON emits Content as either a bare string OR a parts array,
// preserving the assistant tool_calls / tool message fields on either path.
// Empty content is omitted entirely (matching `omitempty` semantics for
// the legacy string field).
func (m openaiMessage) MarshalJSON() ([]byte, error) {
	type baseFields struct {
		Role       string                    `json:"role"`
		Name       string                    `json:"name,omitempty"`
		ToolCallID string                    `json:"tool_call_id,omitempty"`
		ToolCalls  []openaiAssistantToolCall `json:"tool_calls,omitempty"`
	}
	base := baseFields{
		Role:       m.Role,
		Name:       m.Name,
		ToolCallID: m.ToolCallID,
		ToolCalls:  m.ToolCalls,
	}
	if len(m.ContentParts) > 0 {
		type wireWithParts struct {
			baseFields
			Content []openaiContentPart `json:"content"`
		}
		return json.Marshal(wireWithParts{baseFields: base, Content: m.ContentParts})
	}
	if m.Content == "" {
		type wireNoContent struct {
			baseFields
		}
		return json.Marshal(wireNoContent{baseFields: base})
	}
	type wireWithString struct {
		baseFields
		Content string `json:"content"`
	}
	return json.Marshal(wireWithString{baseFields: base, Content: m.Content})
}

// openaiStreamOptions toggles per-chunk metadata. Setting include_usage causes
// the upstream to emit a final chunk with `usage` populated.
type openaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// openaiRequest matches the OpenAI Chat Completions request body that Vertex
// MaaS endpoints accept.
type openaiRequest struct {
	Model         string               `json:"model"`
	Messages      []openaiMessage      `json:"messages"`
	MaxTokens     int32                `json:"max_tokens,omitempty"`
	Temperature   *float32             `json:"temperature,omitempty"`
	TopP          *float32             `json:"top_p,omitempty"`
	Stop          []string             `json:"stop,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	StreamOptions *openaiStreamOptions `json:"stream_options,omitempty"`
	Tools         []openaiToolDef      `json:"tools,omitempty"`
	ToolChoice    *openaiToolChoice    `json:"tool_choice,omitempty"`
}

// translateGenaiToolsToOpenAI converts the gateway-internal tool list into
// the OpenAI Chat Completions `tools` request array. Each
// genai.FunctionDeclaration becomes one openaiToolDef with the parameters
// passed through as a raw JSON Schema object (the format Vertex MaaS expects).
func translateGenaiToolsToOpenAI(in []*genai.Tool) []openaiToolDef {
	var out []openaiToolDef
	for _, t := range in {
		if t == nil {
			continue
		}
		for _, fd := range t.FunctionDeclarations {
			if fd == nil || fd.Name == "" {
				continue
			}
			schema, _ := functionDeclarationSchemaToRaw(fd)
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			out = append(out, openaiToolDef{
				Type: "function",
				Function: openaiToolFunction{
					Name:        fd.Name,
					Description: fd.Description,
					Parameters:  schema,
				},
			})
		}
	}
	return out
}

// translateToolConfigToOpenAI maps a genai.ToolConfig to OpenAI's tool_choice
// slot, plus a boolean indicating that tools should be suppressed entirely.
// Returns nil/false when the config doesn't constrain choice (caller leaves
// the tool_choice field absent, which defaults to "auto").
func translateToolConfigToOpenAI(tc *genai.ToolConfig) (choice *openaiToolChoice, suppress bool) {
	if tc == nil || tc.FunctionCallingConfig == nil {
		return nil, false
	}
	fc := tc.FunctionCallingConfig
	switch fc.Mode {
	case genai.FunctionCallingConfigModeAuto, genai.FunctionCallingConfigModeUnspecified:
		return nil, false
	case genai.FunctionCallingConfigModeAny:
		if len(fc.AllowedFunctionNames) == 1 {
			return &openaiToolChoice{Mode: "function", Name: fc.AllowedFunctionNames[0]}, false
		}
		return &openaiToolChoice{Mode: "required"}, false
	case genai.FunctionCallingConfigModeNone:
		// OpenAI does have a literal "none", but suppressing the `tools`
		// array entirely is more reliable across MaaS publishers (some
		// reject "none" without `tools`).
		return nil, true
	default:
		return nil, false
	}
}

// genaiPartsToOpenAIMessages converts a single genai.Content's parts into
// one or more openaiMessage entries. The mapping is mostly 1:1 but
// FunctionResponse parts become separate role="tool" messages (OpenAI's
// shape — they can't be folded into an assistant turn).
//
// For assistant turns that mix Text + FunctionCall parts, we emit a single
// assistant message with both content and tool_calls populated.
//
// Multimodal: when any InlineData (image) parts are present, the message
// switches from the bare-string Content form to the ContentParts array
// form. Order is preserved across text + image parts so the model sees
// them in the same sequence the caller supplied. Image bytes are
// re-encoded as base64 data: URLs — what OpenAI-compat MaaS endpoints
// (Llama 3.2 Vision, Pixtral, Qwen2-VL, …) accept.
func genaiPartsToOpenAIMessages(role string, parts []*genai.Part) []openaiMessage {
	var (
		out          []openaiMessage
		contentParts []openaiContentPart
		toolCalls    []openaiAssistantToolCall
	)
	// flushMessage emits whatever text/image content + tool_calls we've
	// accumulated so far, picking the right Content vs ContentParts form
	// based on whether any image parts are present.
	flushMessage := func() {
		if len(contentParts) == 0 && len(toolCalls) == 0 {
			return
		}
		msg := openaiMessage{Role: role, ToolCalls: toolCalls}
		if hasAnyImage(contentParts) {
			msg.ContentParts = contentParts
		} else {
			// Collapse text-only parts to a single string so we don't ship
			// the (legal but uglier) parts-array shape for plain-text turns.
			var sb strings.Builder
			for _, cp := range contentParts {
				sb.WriteString(cp.Text)
			}
			msg.Content = sb.String()
		}
		out = append(out, msg)
		contentParts = nil
		toolCalls = nil
	}
	for _, p := range parts {
		if p == nil {
			continue
		}
		switch {
		case p.FunctionResponse != nil:
			// Each FunctionResponse becomes a separate role:"tool" message.
			// We flush any accumulated content first so message ordering is
			// preserved.
			flushMessage()
			out = append(out, openaiMessage{
				Role:       "tool",
				ToolCallID: p.FunctionResponse.ID,
				Name:       p.FunctionResponse.Name,
				Content:    FunctionResponseToText(p.FunctionResponse),
			})
		case p.FunctionCall != nil:
			args, _ := MarshalToolArgs(p.FunctionCall.Args)
			tc := openaiAssistantToolCall{
				ID:   p.FunctionCall.ID,
				Type: "function",
			}
			tc.Function.Name = p.FunctionCall.Name
			tc.Function.Arguments = args
			toolCalls = append(toolCalls, tc)
		case p.InlineData != nil && len(p.InlineData.Data) > 0:
			contentParts = append(contentParts, openaiContentPart{
				Type: "image_url",
				ImageURL: &openaiImageURLObj{
					URL: "data:" + p.InlineData.MIMEType + ";base64," +
						base64.StdEncoding.EncodeToString(p.InlineData.Data),
				},
			})
		case p.Text != "":
			contentParts = append(contentParts, openaiContentPart{
				Type: "text",
				Text: p.Text,
			})
		}
	}
	flushMessage()
	return out
}

// hasAnyImage reports whether any part in the slice is an image_url part.
// Used to decide whether to emit Content (string) or ContentParts (array)
// on the wire.
func hasAnyImage(parts []openaiContentPart) bool {
	for _, p := range parts {
		if p.Type == "image_url" {
			return true
		}
	}
	return false
}

// buildOpenAIRequest converts gateway inputs into the OpenAI body. System
// prompt is prepended as a single role=system message. Tool definitions
// and the tool_choice setting are translated from opts.Tools / opts.ToolConfig.
func buildOpenAIRequest(modelID, systemPrompt string, contents []*genai.Content, opts *GenerationOptions, stream bool) openaiRequest {
	req := openaiRequest{
		Model:  modelID,
		Stream: stream,
	}
	if stream {
		req.StreamOptions = &openaiStreamOptions{IncludeUsage: true}
	}
	if opts != nil {
		req.Temperature = opts.Temperature
		req.TopP = opts.TopP
		req.Stop = opts.Stop
		if opts.MaxTokens != nil && *opts.MaxTokens > 0 {
			req.MaxTokens = *opts.MaxTokens
		}
		choice, suppress := translateToolConfigToOpenAI(opts.ToolConfig)
		if !suppress {
			req.Tools = translateGenaiToolsToOpenAI(opts.Tools)
			if choice != nil {
				req.ToolChoice = choice
			}
		}
	}

	if s := strings.TrimSpace(systemPrompt); s != "" {
		req.Messages = append(req.Messages, openaiMessage{Role: "system", Content: s})
	}
	for _, c := range contents {
		if c == nil {
			continue
		}
		role := "user"
		if c.Role == genai.RoleModel || c.Role == "assistant" {
			role = "assistant"
		}
		req.Messages = append(req.Messages, genaiPartsToOpenAIMessages(role, c.Parts)...)
	}
	return req
}

// openaiUsage matches the `usage` field on chat-completions responses.
type openaiUsage struct {
	PromptTokens     int32 `json:"prompt_tokens"`
	CompletionTokens int32 `json:"completion_tokens"`
	TotalTokens      int32 `json:"total_tokens"`
}

// openaiResponseToolCall is one tool call as returned in a non-streaming
// response. Arguments is a JSON-encoded string per the OpenAI spec.
type openaiResponseToolCall = openaiAssistantToolCall

// openaiResponseMessage is the assistant message returned in a non-stream
// chat-completion choice. Distinct from openaiMessage because the response
// message never carries tool_call_id / name (those are request-only fields)
// but may carry refusal text (which we ignore).
type openaiResponseMessage struct {
	Role      string                   `json:"role"`
	Content   string                   `json:"content"`
	ToolCalls []openaiResponseToolCall `json:"tool_calls,omitempty"`
}

// openaiResponseChoiceDelta is the per-chunk delta on a streaming response.
// Tool calls during streaming arrive as a list of objects keyed on Index
// where each chunk may carry partial fields — the first chunk for a given
// index carries `id` + `function.name`, subsequent chunks for the same
// index append to `function.arguments`. We accumulate per-index.
type openaiResponseChoiceDelta struct {
	Role      string                       `json:"role,omitempty"`
	Content   string                       `json:"content,omitempty"`
	ToolCalls []openaiStreamToolCallDelta  `json:"tool_calls,omitempty"`
}

// openaiStreamToolCallDelta is one entry in a streaming tool_calls delta.
type openaiStreamToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// openaiChoice is one completion choice. We always consume the first one.
type openaiChoice struct {
	Index        int                       `json:"index"`
	Message      openaiResponseMessage     `json:"message"`
	Delta        openaiResponseChoiceDelta `json:"delta"`
	FinishReason string                    `json:"finish_reason"`
}

// openaiResponse is the non-streaming chat-completions response.
type openaiResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   openaiUsage    `json:"usage"`
}

// openaiStreamChunk is a single SSE `data: {...}` payload during streaming.
type openaiStreamChunk struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   *openaiUsage   `json:"usage,omitempty"`
}

// mapOpenAIFinishReason converts OpenAI finish_reason strings into the genai
// FinishReason vocabulary the rest of the codebase expects. "tool_calls"
// maps to our internal FinishReasonToolCalls sentinel.
func mapOpenAIFinishReason(r string) genai.FinishReason {
	switch r {
	case "stop":
		return genai.FinishReasonStop
	case "length":
		return genai.FinishReasonMaxTokens
	case "content_filter":
		return genai.FinishReasonSafety
	case "tool_calls", "function_call":
		return FinishReasonToolCalls
	case "":
		return ""
	default:
		return genai.FinishReasonOther
	}
}

// openaiToolCallsToGenaiParts converts response-side tool calls into the
// genai FunctionCall Part list.
func openaiToolCallsToGenaiParts(calls []openaiResponseToolCall) []*genai.Part {
	var parts []*genai.Part
	for _, tc := range calls {
		args, _ := UnmarshalToolArgs(tc.Function.Arguments)
		parts = append(parts, &genai.Part{
			FunctionCall: &genai.FunctionCall{
				ID:   tc.ID,
				Name: tc.Function.Name,
				Args: args,
			},
		})
	}
	return parts
}

// openaiGenerate executes a non-streaming OpenAI-compatible chat completion
// against the given Vertex publisher (`meta`, `mistralai`, `ai21`, …) and
// adapts the response into a *genai.GenerateContentResponse.
func (vc *VertexClient) openaiGenerate(ctx context.Context, publisher, modelID, systemPrompt string, contents []*genai.Content, opts *GenerationOptions) (*genai.GenerateContentResponse, error) {
	body := buildOpenAIRequest(modelID, systemPrompt, contents, opts, false)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal body: %w", publisher, err)
	}

	url := vc.publisherEndpoint(publisher, modelID, "rawPredict")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", publisher, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if vc.projectID != "" {
		req.Header.Set("x-goog-user-project", vc.projectID)
	}

	resp, err := vc.streamHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: http: %w", publisher, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s: http %d: %s",
			publisher, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	var oresp openaiResponse
	if err := json.NewDecoder(resp.Body).Decode(&oresp); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", publisher, err)
	}

	var (
		text      string
		toolCalls []openaiResponseToolCall
		finishR   string
	)
	if len(oresp.Choices) > 0 {
		ch := oresp.Choices[0]
		text = ch.Message.Content
		toolCalls = ch.Message.ToolCalls
		finishR = ch.FinishReason
	}

	var parts []*genai.Part
	if text != "" {
		parts = append(parts, &genai.Part{Text: text})
	}
	parts = append(parts, openaiToolCallsToGenaiParts(toolCalls)...)
	if len(parts) == 0 {
		parts = []*genai.Part{{Text: ""}}
	}
	finish := mapOpenAIFinishReason(finishR)
	if len(toolCalls) > 0 && finish != FinishReasonToolCalls {
		// Some Vertex MaaS publishers report "stop" even when tool_calls are
		// present; upgrade so downstream sees a coherent finish reason.
		finish = FinishReasonToolCalls
	}

	return &genai.GenerateContentResponse{
		ModelVersion: oresp.Model,
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: parts,
			},
			FinishReason: finish,
		}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     oresp.Usage.PromptTokens,
			CandidatesTokenCount: oresp.Usage.CompletionTokens,
			TotalTokenCount:      oresp.Usage.TotalTokens,
		},
	}, nil
}

// inflightOAIToolCall accumulates a streaming tool_call across multiple
// chunks. OpenAI's stream sends the id + function name on the first delta
// (per index), then appends to function.arguments on subsequent deltas.
type inflightOAIToolCall struct {
	index    int
	id       string
	name     string
	argsBuf  strings.Builder
}

// openaiGenerateStream streams an OpenAI-compatible completion and yields
// per-chunk *genai.GenerateContentResponse values. Text deltas come through
// as Text-part chunks; tool-call deltas accumulate per-index and are
// emitted as FunctionCall chunks when their index is "closed" (either by
// a subsequent finish_reason or end-of-stream).
func (vc *VertexClient) openaiGenerateStream(ctx context.Context, publisher, modelID, systemPrompt string, contents []*genai.Content, opts *GenerationOptions) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		body := buildOpenAIRequest(modelID, systemPrompt, contents, opts, true)
		payload, err := json.Marshal(body)
		if err != nil {
			yield(nil, fmt.Errorf("%s: marshal body: %w", publisher, err))
			return
		}

		url := vc.publisherEndpoint(publisher, modelID, "streamRawPredict")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			yield(nil, fmt.Errorf("%s: build request: %w", publisher, err))
			return
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Accept", "text/event-stream")
		if vc.projectID != "" {
			req.Header.Set("x-goog-user-project", vc.projectID)
		}

		resp, err := vc.streamHTTP.Do(req)
		if err != nil {
			yield(nil, fmt.Errorf("%s: http: %w", publisher, err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			yield(nil, fmt.Errorf("%s: http %d: %s",
				publisher, resp.StatusCode, strings.TrimSpace(string(errBody))))
			return
		}

		var (
			modelVersion  string
			promptTokens  int32
			outputTokens  int32
			totalTokens   int32
			finishR       string
			inflightTools = map[int]*inflightOAIToolCall{}
			sawToolCall   bool
		)

		// flushToolCalls emits a single FunctionCall chunk for every
		// accumulated inflight tool call and clears the map. Called at
		// end-of-stream so the client sees every tool call the model
		// emitted, regardless of whether finish_reason landed first.
		flushToolCalls := func() bool {
			if len(inflightTools) == 0 {
				return true
			}
			// Iterate in index order so multiple parallel calls appear in
			// a deterministic sequence downstream.
			indices := make([]int, 0, len(inflightTools))
			for i := range inflightTools {
				indices = append(indices, i)
			}
			// simple sort — len is tiny (usually 1).
			for i := 0; i < len(indices); i++ {
				for j := i + 1; j < len(indices); j++ {
					if indices[j] < indices[i] {
						indices[i], indices[j] = indices[j], indices[i]
					}
				}
			}
			for _, idx := range indices {
				tc := inflightTools[idx]
				args, _ := UnmarshalToolArgs(tc.argsBuf.String())
				chunk := &genai.GenerateContentResponse{
					ModelVersion: modelVersion,
					Candidates: []*genai.Candidate{{
						Content: &genai.Content{
							Role: genai.RoleModel,
							Parts: []*genai.Part{{
								FunctionCall: &genai.FunctionCall{
									ID:   tc.id,
									Name: tc.name,
									Args: args,
								},
							}},
						},
					}},
				}
				if !yield(chunk, nil) {
					return false
				}
			}
			inflightTools = map[int]*inflightOAIToolCall{}
			return true
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		for scanner.Scan() {
			if ctx.Err() != nil {
				yield(nil, ctx.Err())
				return
			}
			line := scanner.Text()
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}

			var ev openaiStreamChunk
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				// Skip malformed chunks rather than aborting; some publishers
				// emit occasional keep-alive payloads.
				continue
			}
			if ev.Model != "" {
				modelVersion = ev.Model
			}
			if ev.Usage != nil {
				if ev.Usage.PromptTokens > 0 {
					promptTokens = ev.Usage.PromptTokens
				}
				if ev.Usage.CompletionTokens > 0 {
					outputTokens = ev.Usage.CompletionTokens
				}
				if ev.Usage.TotalTokens > 0 {
					totalTokens = ev.Usage.TotalTokens
				}
			}
			if len(ev.Choices) == 0 {
				continue
			}
			ch := ev.Choices[0]
			if ch.FinishReason != "" {
				finishR = ch.FinishReason
			}
			// Tool-call deltas: accumulate by index. The first delta for a
			// given index carries id+name; subsequent deltas append to args.
			for _, tcd := range ch.Delta.ToolCalls {
				sawToolCall = true
				tc, ok := inflightTools[tcd.Index]
				if !ok {
					tc = &inflightOAIToolCall{index: tcd.Index}
					inflightTools[tcd.Index] = tc
				}
				if tcd.ID != "" {
					tc.id = tcd.ID
				}
				if tcd.Function.Name != "" {
					tc.name = tcd.Function.Name
				}
				if tcd.Function.Arguments != "" {
					tc.argsBuf.WriteString(tcd.Function.Arguments)
				}
			}
			if ch.Delta.Content == "" {
				continue
			}
			chunk := &genai.GenerateContentResponse{
				ModelVersion: modelVersion,
				Candidates: []*genai.Candidate{{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: ch.Delta.Content}},
					},
				}},
			}
			if !yield(chunk, nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil && err != io.EOF {
			yield(nil, fmt.Errorf("%s: read stream: %w", publisher, err))
			return
		}

		// Flush any inflight tool calls before the synthetic final chunk so
		// the client sees them in order.
		if !flushToolCalls() {
			return
		}

		if totalTokens == 0 {
			totalTokens = promptTokens + outputTokens
		}
		finish := mapOpenAIFinishReason(finishR)
		if sawToolCall && finish != FinishReasonToolCalls {
			finish = FinishReasonToolCalls
		}
		final := &genai.GenerateContentResponse{
			ModelVersion: modelVersion,
			Candidates: []*genai.Candidate{{
				Content: &genai.Content{
					Role:  genai.RoleModel,
					Parts: []*genai.Part{},
				},
				FinishReason: finish,
			}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     promptTokens,
				CandidatesTokenCount: outputTokens,
				TotalTokenCount:      totalTokens,
			},
		}
		yield(final, nil)
	}
}
