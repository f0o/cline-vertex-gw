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

// Anthropic models on Vertex AI are NOT served via the Gemini-flavored
// `:streamGenerateContent` endpoint. They must be addressed using Anthropic's
// native Messages API shape against `:rawPredict` / `:streamRawPredict`.
//
// This file implements that adapter and exposes the result as
// `*genai.GenerateContentResponse` so the rest of the gateway is unchanged.

// defaultAnthropicMaxTokens is used when the caller did not supply a maximum.
// The Anthropic Messages API requires a value, unlike Gemini.
const defaultAnthropicMaxTokens = 65536

// anthropicVersion is the on-Vertex Anthropic API version tag.
const anthropicVersion = "vertex-2023-10-16"

// isAnthropicReasoningModel reports whether the given Anthropic model id
// belongs to the "extended thinking" / reasoning family, which rejects the
// `temperature`, `top_p`, and `top_k` sampling parameters with HTTP 400.
func isAnthropicReasoningModel(modelID string) bool {
	id := strings.ToLower(modelID)
	switch {
	case strings.HasPrefix(id, "claude-opus-4"):
		return true
	case strings.HasPrefix(id, "claude-sonnet-4"):
		return true
	case strings.Contains(id, "-thinking"):
		return true
	}
	return false
}

// anthropicCacheControl is the cache-tagging marker recognized by Anthropic's
// Messages API. Attaching it to a content block tells Anthropic to cache the
// prefix up to and including that block.
type anthropicCacheControl struct {
	Type string `json:"type"` // currently always "ephemeral"
}

// anthropicBlock is one element of a structured `content` array. The Type
// field selects which of the optional payload fields is meaningful on the
// wire. This is the polymorphic shape Anthropic uses for assistant turns that
// mix free-text reasoning with tool_use invocations, and for user turns that
// carry tool_result envelopes.
//
// Marshaling uses omitempty on every optional payload so a Type="text" block
// emits only {type,text[,cache_control]}, a Type="tool_use" block emits
// {type,id,name,input[,cache_control]}, and a Type="tool_result" block emits
// {type,tool_use_id,content[,is_error,cache_control]} — matching Anthropic's
// per-block-type wire shape exactly.
type anthropicBlock struct {
	Type         string                 `json:"type"`                    // "text" | "image" | "tool_use" | "tool_result"
	Text         string                 `json:"text,omitempty"`          // text blocks
	Source       *anthropicImageSource  `json:"source,omitempty"`        // image blocks
	ID           string                 `json:"id,omitempty"`            // tool_use: assistant-emitted call id
	Name         string                 `json:"name,omitempty"`          // tool_use: tool name to invoke
	Input        json.RawMessage        `json:"input,omitempty"`         // tool_use: arguments JSON object
	ToolUseID    string                 `json:"tool_use_id,omitempty"`   // tool_result: id of the tool_use this answers
	Content      json.RawMessage        `json:"content,omitempty"`       // tool_result: result body (string or array)
	IsError      bool                   `json:"is_error,omitempty"`      // tool_result: true if the tool reported an error
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"` // any block: prompt-cache marker
}

// anthropicImageSource is the nested `source` object on an image block. We
// only ever emit the base64 form (Anthropic also accepts a "url" type but we
// don't have a use case for it — Cline ships images inline). MediaType is
// MUST-be-set for base64 sources; the four image MIMEs Claude accepts are
// image/jpeg, image/png, image/gif, image/webp — exactly the set our
// supportedImageMIMETypes allowlist enforces in api/multimodal.go.
type anthropicImageSource struct {
	Type      string `json:"type"`       // always "base64"
	MediaType string `json:"media_type"` // "image/png" | "image/jpeg" | "image/gif" | "image/webp"
	Data      string `json:"data"`       // base64-encoded image bytes, no `data:` prefix
}

// anthropicTextBlock is the historical name for what is now anthropicBlock.
// Retained as an alias so existing tests that construct text-only blocks
// (`anthropicTextBlock{Type:"text", Text:..., CacheControl:...}`) continue
// to compile unchanged.
type anthropicTextBlock = anthropicBlock

// anthropicMessage is one entry in the Messages API payload. The `Content`
// field is encoded as either a bare string (legacy) or a content-block array
// (required when any block needs cache_control, or when the message carries
// tool_use / tool_result content). Custom MarshalJSON below selects the
// correct shape based on whether Blocks is populated.
type anthropicMessage struct {
	Role    string           `json:"role"`
	Content string           `json:"-"` // legacy plain-text path
	Blocks  []anthropicBlock `json:"-"` // structured path
}

// MarshalJSON emits either `"content": "..."` (when Blocks is empty) or
// `"content": [{...}, ...]` (when Blocks is populated).
func (m anthropicMessage) MarshalJSON() ([]byte, error) {
	type wire struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	w := wire{Role: m.Role}
	if len(m.Blocks) > 0 {
		w.Content = m.Blocks
	} else {
		w.Content = m.Content
	}
	return json.Marshal(w)
}

// anthropicSystem is the system-prompt slot. Like `content`, it can be a bare
// string or a block array; we always emit blocks when caching is enabled so
// we have somewhere to hang `cache_control`.
type anthropicSystem struct {
	Text   string           // legacy plain-text path
	Blocks []anthropicBlock // structured path (preferred when caching)
}

// MarshalJSON: collapse to a single string when no blocks are set; otherwise
// emit the block array.
func (s anthropicSystem) MarshalJSON() ([]byte, error) {
	if len(s.Blocks) > 0 {
		return json.Marshal(s.Blocks)
	}
	return json.Marshal(s.Text)
}

func (s anthropicSystem) isEmpty() bool {
	return s.Text == "" && len(s.Blocks) == 0
}

// anthropicTool is one entry in the request's `tools` array. The `input_schema`
// is a JSON-Schema-shaped object (anthropic accepts an OpenAPI 3 subset).
// We serialize it as RawMessage so we can pass whatever the caller already
// produced — either FunctionDeclaration.ParametersJsonSchema (raw JSON, the
// common path for OpenAI-shape inputs) or a marshaled FunctionDeclaration.
// Parameters (typed via genai.Schema) — without re-shaping it.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicToolChoice selects the model's tool_choice behavior:
//   - {type:"auto"}                       — default; model may answer or call
//   - {type:"any"}                        — model MUST call SOME tool
//   - {type:"tool", name:"<fn>"}          — model MUST call this specific tool
//   - {type:"none"}                       — model MUST NOT call a tool
//
// We marshal Name only when Type=="tool".
type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// anthropicRequest matches the Messages-API body that Vertex's Anthropic
// endpoint accepts.
type anthropicRequest struct {
	AnthropicVersion string               `json:"anthropic_version"`
	Messages         []anthropicMessage   `json:"messages"`
	System           *anthropicSystem     `json:"system,omitempty"`
	MaxTokens        int32                `json:"max_tokens"`
	Temperature      *float32             `json:"temperature,omitempty"`
	TopP             *float32             `json:"top_p,omitempty"`
	TopK             *int32               `json:"top_k,omitempty"`
	StopSequences    []string             `json:"stop_sequences,omitempty"`
	Stream           bool                 `json:"stream,omitempty"`
	Tools            []anthropicTool      `json:"tools,omitempty"`
	ToolChoice       *anthropicToolChoice `json:"tool_choice,omitempty"`
}

// translateGenaiToolsToAnthropic converts the gateway-internal []*genai.Tool
// list into Anthropic's `tools` request array.
//
// Each genai.FunctionDeclaration becomes one anthropicTool. The input_schema
// is supplied either from FunctionDeclaration.ParametersJsonSchema (when the
// caller provided raw JSON — the common path when we got the request from
// the OpenAI surface) or by JSON-marshaling FunctionDeclaration.Parameters
// (the typed genai.Schema). When neither is set we emit `{"type":"object"}`
// because Anthropic requires a non-empty schema.
//
// Tools without FunctionDeclarations (Google-specific built-ins like
// GoogleSearch, CodeExecution, etc.) are silently dropped because Anthropic
// has no equivalent. If after filtering nothing remains we return nil so the
// caller can omit the Tools field entirely.
func translateGenaiToolsToAnthropic(in []*genai.Tool) []anthropicTool {
	var out []anthropicTool
	for _, t := range in {
		if t == nil {
			continue
		}
		for _, fd := range t.FunctionDeclarations {
			if fd == nil || fd.Name == "" {
				continue
			}
			schema, err := functionDeclarationSchemaToRaw(fd)
			if err != nil || len(schema) == 0 {
				// Fallback: an empty-object schema. Anthropic requires
				// input_schema to be present; "any-shape object" lets the
				// model still try to call the tool.
				schema = json.RawMessage(`{"type":"object"}`)
			}
			out = append(out, anthropicTool{
				Name:        fd.Name,
				Description: fd.Description,
				InputSchema: schema,
			})
		}
	}
	return out
}

// functionDeclarationSchemaToRaw extracts an input-schema JSON blob from a
// genai.FunctionDeclaration, preferring the ParametersJsonSchema field when
// present (it's already JSON-shaped — typical for inputs we translated from
// OpenAI's tool format) and otherwise marshaling the typed Parameters.
func functionDeclarationSchemaToRaw(fd *genai.FunctionDeclaration) (json.RawMessage, error) {
	if fd.ParametersJsonSchema != nil {
		switch v := fd.ParametersJsonSchema.(type) {
		case json.RawMessage:
			return v, nil
		case []byte:
			return json.RawMessage(v), nil
		case string:
			return json.RawMessage(v), nil
		default:
			b, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			return json.RawMessage(b), nil
		}
	}
	if fd.Parameters != nil {
		b, err := json.Marshal(fd.Parameters)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(b), nil
	}
	return nil, nil
}

// translateToolConfigToAnthropic maps a genai.ToolConfig (the gateway-internal
// tool_choice representation) onto Anthropic's anthropicToolChoice. Returns
// nil when the config doesn't constrain choice (Anthropic's default is "auto"
// when `tool_choice` is omitted), and also returns a boolean indicating
// whether the caller asked for "none" — which means we must drop the `tools`
// array entirely (Anthropic has no literal `{type:"none"}`).
func translateToolConfigToAnthropic(tc *genai.ToolConfig) (choice *anthropicToolChoice, suppressTools bool) {
	if tc == nil || tc.FunctionCallingConfig == nil {
		return nil, false
	}
	fc := tc.FunctionCallingConfig
	switch fc.Mode {
	case genai.FunctionCallingConfigModeAuto, genai.FunctionCallingConfigModeUnspecified:
		return nil, false // default "auto"
	case genai.FunctionCallingConfigModeAny:
		if len(fc.AllowedFunctionNames) == 1 {
			return &anthropicToolChoice{Type: "tool", Name: fc.AllowedFunctionNames[0]}, false
		}
		return &anthropicToolChoice{Type: "any"}, false
	case genai.FunctionCallingConfigModeNone:
		return nil, true
	default:
		return nil, false
	}
}

// extractAnthropicBlocksFromParts walks a genai.Content's Parts and returns
// the corresponding Anthropic block list. Text parts become "text" blocks;
// InlineData (image) parts become "image" blocks with a base64 source;
// FunctionCall parts become "tool_use" blocks; FunctionResponse parts become
// "tool_result" blocks. Order is preserved so a mixed assistant turn
// (text reasoning + tool_use) or a multimodal user turn (text + image)
// round-trips faithfully.
func extractAnthropicBlocksFromParts(parts []*genai.Part) []anthropicBlock {
	var blocks []anthropicBlock
	for _, p := range parts {
		if p == nil {
			continue
		}
		switch {
		case p.FunctionCall != nil:
			argsJSON, _ := MarshalToolArgs(p.FunctionCall.Args)
			blocks = append(blocks, anthropicBlock{
				Type:  "tool_use",
				ID:    p.FunctionCall.ID,
				Name:  p.FunctionCall.Name,
				Input: json.RawMessage(argsJSON),
			})
		case p.FunctionResponse != nil:
			body := FunctionResponseToText(p.FunctionResponse)
			contentJSON, _ := json.Marshal(body)
			isErr := false
			if _, ok := p.FunctionResponse.Response["error"]; ok {
				if _, hasOut := p.FunctionResponse.Response["output"]; !hasOut {
					isErr = true
				}
			}
			blocks = append(blocks, anthropicBlock{
				Type:      "tool_result",
				ToolUseID: p.FunctionResponse.ID,
				Content:   contentJSON,
				IsError:   isErr,
			})
		case p.InlineData != nil && len(p.InlineData.Data) > 0:
			// Anthropic requires the bytes to be base64-encoded on the
			// wire (not raw binary in JSON, obviously). The genai SDK
			// stores them as []byte; we re-encode here.
			blocks = append(blocks, anthropicBlock{
				Type: "image",
				Source: &anthropicImageSource{
					Type:      "base64",
					MediaType: p.InlineData.MIMEType,
					Data:      base64.StdEncoding.EncodeToString(p.InlineData.Data),
				},
			})
		case p.Text != "":
			blocks = append(blocks, anthropicBlock{
				Type: "text",
				Text: p.Text,
			})
		}
	}
	return blocks
}

// blocksAreTextOnly reports whether every block in the slice is a plain
// "text" block (so the caller can decide whether the legacy string-content
// wire form is still valid).
func blocksAreTextOnly(blocks []anthropicBlock) bool {
	for _, b := range blocks {
		if b.Type != "text" {
			return false
		}
	}
	return true
}

// rawMsg is an internal staging type used while building the Anthropic
// messages array. It separates plain-text turns (which we may collapse to
// the legacy bare-string wire form) from blocked turns (which carry
// tool_use / tool_result and therefore must use the structured form).
type rawMsg struct {
	role   string
	text   string           // populated for legacy plain-text turns
	blocks []anthropicBlock // populated when this turn carries any non-text part
}

func (r rawMsg) hasBlocks() bool { return len(r.blocks) > 0 }

// buildAnthropicRequest converts genai-flavored inputs into the Anthropic
// payload. Roles are translated (`model` -> `assistant`), text parts within
// a single Content block are concatenated, and a default max_tokens is
// applied.
//
// Tool calling: when opts.Tools is non-empty the tools are translated to
// Anthropic's `tools` shape and opts.ToolConfig (if any) is mapped to
// `tool_choice`. Conversation history that carries genai.FunctionCall /
// FunctionResponse parts is translated into Anthropic tool_use / tool_result
// content blocks so multi-turn tool conversations survive the round-trip.
//
// For reasoning-class models (see isAnthropicReasoningModel) the sampling
// parameters `temperature`, `top_p`, and `top_k` are deliberately omitted
// because the upstream rejects them with a 400.
//
// Prompt caching: the system prompt and the FIRST user turn (in multi-turn
// calls) are tagged with `cache_control: {"type":"ephemeral"}` when the
// prefix meets the size threshold (~4 KB ≈ 1100 tokens). Caching is a no-op
// when the prefix is below Anthropic's minimum cacheable size.
func buildAnthropicRequest(modelID, systemPrompt string, contents []*genai.Content, opts *GenerationOptions, stream bool) anthropicRequest {
	req := anthropicRequest{
		AnthropicVersion: anthropicVersion,
		MaxTokens:        defaultAnthropicMaxTokens,
		Stream:           stream,
	}
	reasoning := isAnthropicReasoningModel(modelID)
	var (
		tools         []anthropicTool
		toolChoice    *anthropicToolChoice
		suppressTools bool
	)
	if opts != nil {
		if !reasoning {
			req.Temperature = opts.Temperature
			req.TopP = opts.TopP
			req.TopK = opts.TopK
		}
		req.StopSequences = opts.Stop
		if opts.MaxTokens != nil && *opts.MaxTokens > 0 {
			req.MaxTokens = *opts.MaxTokens
		}
		tools = translateGenaiToolsToAnthropic(opts.Tools)
		toolChoice, suppressTools = translateToolConfigToAnthropic(opts.ToolConfig)
	}
	if !suppressTools && len(tools) > 0 {
		req.Tools = tools
		if toolChoice != nil {
			req.ToolChoice = toolChoice
		}
	}

	// Translate genai Contents -> staged messages. Same-role consecutive
	// turns are merged: text parts are concatenated and any tool_use /
	// tool_result parts force the merged turn into structured (blocks) form.
	var raws []rawMsg
	for _, c := range contents {
		if c == nil {
			continue
		}
		role := "user"
		if c.Role == genai.RoleModel || c.Role == "assistant" {
			role = "assistant"
		}
		blocks := extractAnthropicBlocksFromParts(c.Parts)
		if len(blocks) == 0 {
			continue
		}
		if n := len(raws); n > 0 && raws[n-1].role == role {
			prev := raws[n-1]
			if prev.hasBlocks() || !blocksAreTextOnly(blocks) {
				if !prev.hasBlocks() && prev.text != "" {
					prev.blocks = []anthropicBlock{{Type: "text", Text: prev.text}}
					prev.text = ""
				}
				prev.blocks = append(prev.blocks, blocks...)
			} else {
				// Both sides are plain text — concatenate.
				for _, b := range blocks {
					prev.text += b.Text
				}
			}
			raws[n-1] = prev
			continue
		}
		if !blocksAreTextOnly(blocks) {
			raws = append(raws, rawMsg{role: role, blocks: blocks})
		} else {
			var sb strings.Builder
			for _, b := range blocks {
				sb.WriteString(b.Text)
			}
			raws = append(raws, rawMsg{role: role, text: sb.String()})
		}
	}

	sys := strings.TrimSpace(systemPrompt)

	// Prompt-cache breakpoints are decided by the shared, provider-agnostic
	// planner (PlanCache) so the same write-vs-read economics apply across
	// every publisher. The plan's anchors are expressed in merged-turn order,
	// which is exactly the order of `raws` below — so anchor indices line up
	// 1:1. PlanCache is called with the ORIGINAL contents (its mergedTurns
	// helper performs the same same-role merging this function does), so the
	// returned indices match.
	//
	// The cardinal rule (never cache a prefix with no later turn to read it
	// back) is enforced inside PlanCache; here we simply honor its decisions.
	plan := PlanCache(contents, sys, "anthropic")

	if plan.CacheSystem {
		req.System = &anthropicSystem{
			Blocks: []anthropicBlock{{
				Type:         "text",
				Text:         sys,
				CacheControl: &anthropicCacheControl{Type: "ephemeral"},
			}},
		}
	} else if sys != "" {
		req.System = &anthropicSystem{Text: sys}
	}

	// tagTurn attaches a cache_control marker to the LAST block of a turn so
	// the cached prefix encompasses every preceding block. Plain-text turns are
	// promoted to a single-block form so they have somewhere to hang the marker.
	tagTurn := func(msg *anthropicMessage, m rawMsg) {
		if m.hasBlocks() {
			msg.Blocks = m.blocks
			msg.Blocks[len(msg.Blocks)-1].CacheControl = &anthropicCacheControl{Type: "ephemeral"}
			return
		}
		msg.Blocks = []anthropicBlock{{
			Type:         "text",
			Text:         m.text,
			CacheControl: &anthropicCacheControl{Type: "ephemeral"},
		}}
	}

	for i, m := range raws {
		var msg anthropicMessage
		msg.Role = m.role
		switch {
		case i == plan.FirstUserTurnIdx || i == plan.TailTurnIdx:
			// Cache breakpoint anchor (first-user head or rolling tail).
			tagTurn(&msg, m)
		case m.hasBlocks():
			msg.Blocks = m.blocks
		default:
			msg.Content = m.text
		}
		req.Messages = append(req.Messages, msg)
	}

	// Defensive: if we end up with no payload at all, drop the System pointer
	// so omitempty hides it on the wire.
	if req.System != nil && req.System.isEmpty() {
		req.System = nil
	}
	return req
}

// anthropicUsage matches the `usage` field returned by Anthropic responses.
//
// The cache-related fields are populated only when prompt caching is in use:
//   - CacheCreationInputTokens: tokens that were just written into the cache
//     by this request (a one-time premium of ~125% of normal input rate).
//   - CacheReadInputTokens:     tokens served from a previous request's cache
//     (charged at ~10% of normal input rate — the savings).
type anthropicUsage struct {
	InputTokens              int32 `json:"input_tokens"`
	OutputTokens             int32 `json:"output_tokens"`
	CacheCreationInputTokens int32 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int32 `json:"cache_read_input_tokens"`
}

// anthropicResponseBlock is one element of the `content` array on a non-stream
// response. Like anthropicBlock for requests, the Type field selects which
// optional payload fields are meaningful. For responses we only ever see
// "text" and "tool_use" (tool_result is request-only).
type anthropicResponseBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// anthropicResponse is the non-streaming `messages` response.
type anthropicResponse struct {
	ID         string                   `json:"id"`
	Type       string                   `json:"type"`
	Role       string                   `json:"role"`
	Model      string                   `json:"model"`
	Content    []anthropicResponseBlock `json:"content"`
	StopReason string                   `json:"stop_reason"`
	Usage      anthropicUsage           `json:"usage"`
}

// mapAnthropicStopReason converts Anthropic's stop_reason vocabulary into the
// genai FinishReason vocabulary the rest of the codebase expects. The
// "tool_use" stop reason maps to our internal FinishReasonToolCalls sentinel
// so the OpenAI / Ollama surface emitters can translate it to "tool_calls" /
// "tool_use" on their respective wires.
func mapAnthropicStopReason(r string) genai.FinishReason {
	switch r {
	case "end_turn", "stop_sequence":
		return genai.FinishReasonStop
	case "max_tokens":
		return genai.FinishReasonMaxTokens
	case "tool_use":
		return FinishReasonToolCalls
	case "":
		return ""
	default:
		return genai.FinishReasonOther
	}
}

// anthropicResponseToGenaiParts translates the structured `content` array on
// an Anthropic response into the genai-flavored Part list the rest of the
// gateway consumes. Text blocks become Text parts; tool_use blocks become
// FunctionCall parts with the `input` JSON decoded into Args.
//
// Malformed `input` JSON is tolerated: the resulting FunctionCall.Args is
// left nil rather than aborting (the upstream guarantees valid JSON in
// practice; the defensive fallback is only here so a transient publisher
// glitch doesn't tank the whole request).
func anthropicResponseToGenaiParts(blocks []anthropicResponseBlock) []*genai.Part {
	var parts []*genai.Part
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			parts = append(parts, &genai.Part{Text: b.Text})
		case "tool_use":
			args, _ := UnmarshalToolArgs(string(b.Input))
			parts = append(parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{
					ID:   b.ID,
					Name: b.Name,
					Args: args,
				},
			})
		}
	}
	return parts
}

// anthropicGenerate executes a non-streaming Anthropic Messages call and
// adapts the response into a *genai.GenerateContentResponse.
func (vc *VertexClient) anthropicGenerate(ctx context.Context, modelID, systemPrompt string, contents []*genai.Content, opts *GenerationOptions) (*genai.GenerateContentResponse, error) {
	body := buildAnthropicRequest(modelID, systemPrompt, contents, opts, false)
	DebugLogPayload(ctx, "upstream_request", body)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal body: %w", err)
	}

	url := vc.publisherEndpoint("anthropic", modelID, "rawPredict")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if vc.projectID != "" {
		req.Header.Set("x-goog-user-project", vc.projectID)
	}

	resp, err := vc.streamHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("anthropic: http %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	var aresp anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&aresp); err != nil {
		return nil, fmt.Errorf("anthropic: decode: %w", err)
	}
	DebugLogPayload(ctx, "upstream_response", aresp)

	// Mirror cached-token accounting onto the genai shape. Cache-read tokens
	// are folded into PromptTokenCount because Anthropic excludes them from
	// `input_tokens`; downstream rate-limit / cost views should still see them
	// as input. CachedContentTokenCount preserves the breakdown for telemetry.
	totalPrompt := aresp.Usage.InputTokens + aresp.Usage.CacheReadInputTokens + aresp.Usage.CacheCreationInputTokens
	parts := anthropicResponseToGenaiParts(aresp.Content)
	if len(parts) == 0 {
		// Anthropic guarantees at least one block, but keep the candidate
		// non-empty so downstream Part-iteration doesn't NPE.
		parts = []*genai.Part{{Text: ""}}
	}
	return &genai.GenerateContentResponse{
		ModelVersion: aresp.Model,
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: parts,
			},
			FinishReason: mapAnthropicStopReason(aresp.StopReason),
		}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        totalPrompt,
			CandidatesTokenCount:    aresp.Usage.OutputTokens,
			CachedContentTokenCount: aresp.Usage.CacheReadInputTokens,
			TotalTokenCount:         totalPrompt + aresp.Usage.OutputTokens,
		},
	}, nil
}

// SSE event payloads we care about during streaming. Anthropic's stream uses
// distinct event names for the different chunk types; each event carries a
// JSON `data:` payload whose shape depends on the event.

type sseMessageStart struct {
	Message struct {
		ID    string         `json:"id"`
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
}

type sseContentBlockStart struct {
	Index        int `json:"index"`
	ContentBlock struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
		Text  string          `json:"text"`
	} `json:"content_block"`
}

type sseContentBlockDelta struct {
	Index int `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
}

type sseMessageDelta struct {
	Delta struct {
		StopReason   string `json:"stop_reason"`
		StopSequence string `json:"stop_sequence"`
	} `json:"delta"`
	Usage anthropicUsage `json:"usage"`
}

// inflightToolCall tracks an in-progress tool_use block as we accumulate its
// streaming `input_json_delta` chunks. Once the matching content_block_stop
// arrives we parse the buffered JSON and emit a single genai FunctionCall
// part downstream.
type inflightToolCall struct {
	index   int
	id      string
	name    string
	argsBuf strings.Builder
}

// anthropicGenerateStream issues a `streamRawPredict` call and yields
// per-chunk `*genai.GenerateContentResponse` values. Text deltas come through
// as chunks containing Text parts; tool_use blocks accumulate their streamed
// JSON arguments across content_block_delta events and are emitted as a
// single FunctionCall chunk when their content_block_stop arrives. A final
// synthetic chunk carrying FinishReason + UsageMetadata is yielded at
// end-of-stream.
func (vc *VertexClient) anthropicGenerateStream(ctx context.Context, modelID, systemPrompt string, contents []*genai.Content, opts *GenerationOptions) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		body := buildAnthropicRequest(modelID, systemPrompt, contents, opts, true)
		DebugLogPayload(ctx, "upstream_request", body)
		payload, err := json.Marshal(body)
		if err != nil {
			yield(nil, fmt.Errorf("anthropic: marshal body: %w", err))
			return
		}

		url := vc.publisherEndpoint("anthropic", modelID, "streamRawPredict")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			yield(nil, fmt.Errorf("anthropic: build request: %w", err))
			return
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Accept", "text/event-stream")
		if vc.projectID != "" {
			req.Header.Set("x-goog-user-project", vc.projectID)
		}

		resp, err := vc.streamHTTP.Do(req)
		if err != nil {
			yield(nil, fmt.Errorf("anthropic: http: %w", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			yield(nil, fmt.Errorf("anthropic: http %d: %s",
				resp.StatusCode, strings.TrimSpace(string(errBody))))
			return
		}

		var (
			modelVersion        string
			promptTokens        int32
			outputTokens        int32
			cacheReadTokens     int32
			cacheCreationTokens int32
			stopReason          string
			// inflightTools maps a content-block index to the in-progress
			// tool_use being accumulated for that block. We need per-index
			// because Anthropic interleaves multiple parallel tool_use blocks
			// in a single response (when the model invokes >1 tool at once).
			inflightTools = map[int]*inflightToolCall{}
		)

		scanner := bufio.NewScanner(resp.Body)
		// Allow up to 1 MiB per SSE line; default is 64 KiB which is too small
		// for occasional larger events (e.g. tool call payloads).
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		// emitToolCall flushes an inflight tool_use as a FunctionCall chunk
		// downstream. Buffered argsBuf may be empty (parameterless tool) or
		// malformed (an upstream glitch); in both cases we still emit the
		// FunctionCall so the client can react — with nil Args for the
		// parameterless case and best-effort Args=nil for the malformed one.
		emitToolCall := func(tc *inflightToolCall) bool {
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
			return yield(chunk, nil)
		}

		var eventName string
		for scanner.Scan() {
			if ctx.Err() != nil {
				yield(nil, ctx.Err())
				return
			}
			line := scanner.Text()
			if line == "" {
				eventName = ""
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue // SSE comment / keep-alive
			}
			if strings.HasPrefix(line, "event:") {
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			DebugLogPayload(ctx, "upstream_response_chunk", map[string]string{"event": eventName, "data": data})

			switch eventName {
			case "message_start":
				var ev sseMessageStart
				if err := json.Unmarshal([]byte(data), &ev); err == nil {
					modelVersion = ev.Message.Model
					if ev.Message.Usage.InputTokens > 0 {
						promptTokens = ev.Message.Usage.InputTokens
					}
					if ev.Message.Usage.OutputTokens > 0 {
						outputTokens = ev.Message.Usage.OutputTokens
					}
					if ev.Message.Usage.CacheReadInputTokens > 0 {
						cacheReadTokens = ev.Message.Usage.CacheReadInputTokens
					}
					if ev.Message.Usage.CacheCreationInputTokens > 0 {
						cacheCreationTokens = ev.Message.Usage.CacheCreationInputTokens
					}
				}
			case "content_block_start":
				var ev sseContentBlockStart
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					continue
				}
				if ev.ContentBlock.Type == "tool_use" {
					// Begin accumulating a new tool_use. The `input` field on
					// the start event is typically `{}` (the JSON args are
					// streamed via subsequent input_json_delta events); we
					// seed argsBuf with it just in case the upstream sent
					// the complete args inline on the start event.
					tc := &inflightToolCall{
						index: ev.Index,
						id:    ev.ContentBlock.ID,
						name:  ev.ContentBlock.Name,
					}
					if seed := strings.TrimSpace(string(ev.ContentBlock.Input)); seed != "" && seed != "{}" {
						tc.argsBuf.WriteString(seed)
					}
					inflightTools[ev.Index] = tc
				}
			case "content_block_delta":
				var ev sseContentBlockDelta
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					continue
				}
				switch ev.Delta.Type {
				case "text_delta":
					if ev.Delta.Text == "" {
						continue
					}
					chunk := &genai.GenerateContentResponse{
						ModelVersion: modelVersion,
						Candidates: []*genai.Candidate{{
							Content: &genai.Content{
								Role:  genai.RoleModel,
								Parts: []*genai.Part{{Text: ev.Delta.Text}},
							},
						}},
					}
					if !yield(chunk, nil) {
						return
					}
				case "input_json_delta":
					if tc, ok := inflightTools[ev.Index]; ok {
						tc.argsBuf.WriteString(ev.Delta.PartialJSON)
					}
				}
			case "content_block_stop":
				// Generic { "type": "content_block_stop", "index": N } — flush
				// the in-progress tool_use if any.
				var ev struct {
					Index int `json:"index"`
				}
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					continue
				}
				if tc, ok := inflightTools[ev.Index]; ok {
					delete(inflightTools, ev.Index)
					if !emitToolCall(tc) {
						return
					}
				}
			case "message_delta":
				var ev sseMessageDelta
				if err := json.Unmarshal([]byte(data), &ev); err == nil {
					if ev.Delta.StopReason != "" {
						stopReason = ev.Delta.StopReason
					}
					if ev.Usage.OutputTokens > 0 {
						outputTokens = ev.Usage.OutputTokens
					}
					if ev.Usage.InputTokens > 0 {
						promptTokens = ev.Usage.InputTokens
					}
					if ev.Usage.CacheReadInputTokens > 0 {
						cacheReadTokens = ev.Usage.CacheReadInputTokens
					}
					if ev.Usage.CacheCreationInputTokens > 0 {
						cacheCreationTokens = ev.Usage.CacheCreationInputTokens
					}
				}
			case "error":
				yield(nil, fmt.Errorf("anthropic stream error: %s", data))
				return
			}
		}
		if err := scanner.Err(); err != nil && err != io.EOF {
			yield(nil, fmt.Errorf("anthropic: read stream: %w", err))
			return
		}

		// Defensive: if the upstream closed without a content_block_stop for
		// an inflight tool_use, emit whatever we accumulated rather than
		// dropping the call silently. This can happen if the stream is cut
		// mid-flight — we'd rather surface a partial call (which downstream
		// can flag as malformed) than silently lose the model's intent.
		for _, tc := range inflightTools {
			if !emitToolCall(tc) {
				return
			}
		}

		// Fold cached/just-cached tokens into PromptTokenCount so usage stays
		// faithful to "total input tokens billed" — keep the cache-read count
		// surfaced separately via CachedContentTokenCount for cost telemetry.
		totalPrompt := promptTokens + cacheReadTokens + cacheCreationTokens

		// Final synthetic chunk carrying finish reason + usage. Mirrors how
		// Gemini's last streamed chunk includes usageMetadata + finishReason.
		final := &genai.GenerateContentResponse{
			ModelVersion: modelVersion,
			Candidates: []*genai.Candidate{{
				Content: &genai.Content{
					Role:  genai.RoleModel,
					Parts: []*genai.Part{},
				},
				FinishReason: mapAnthropicStopReason(stopReason),
			}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:        totalPrompt,
				CandidatesTokenCount:    outputTokens,
				CachedContentTokenCount: cacheReadTokens,
				TotalTokenCount:         totalPrompt + outputTokens,
			},
		}
		yield(final, nil)
	}
}
