package provider

import (
	"go.f0o.dev/cline-vertex-gw/pkg/pipeline"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"

	"google.golang.org/genai"
)

// Cohere's Command R / R+ models on Vertex AI use Cohere's native `/chat`
// shape against `:rawPredict` / `:streamRawPredict` (NOT OpenAI-compatible,
// despite Cohere also offering an OpenAI-compatible mode on their direct API).
//
// Body shape (the relevant fields for our use):
//
//	{
//	  "message":      "<latest user message>",   // current turn, single string
//	  "chat_history": [                          // prior turns
//	    {"role": "USER",      "message": "..."},
//	    {"role": "CHATBOT",   "message": "...",
//	     "tool_calls": [{"name":"fn","parameters":{...}}]},
//	    {"role": "TOOL",      "tool_results": [...]}
//	  ],
//	  "preamble":     "<system prompt>",         // optional
//	  "stream":       true,                      // for streamRawPredict
//	  "max_tokens":   1024,
//	  "temperature":  0.3,
//	  "p":            0.9,                       // top-p
//	  "k":            40,                        // top-k
//	  "stop_sequences": ["..."],
//	  "tools": [
//	    {"name":"<fn>",
//	     "description":"...",
//	     "parameter_definitions": {
//	       "<param>": {"description":"...", "type":"str", "required":true}
//	     }}
//	  ],
//	  "tool_results": [                          // for the CURRENT turn only;
//	    {"call":   {"name":"<fn>","parameters":{...}},  // anything in history
//	     "outputs":[{"output":"..."}]}                  // goes in chat_history
//	  ],
//	  "force_single_step": true                  // optional; we leave default
//	}
//
// Stream events are JSONL-shaped SSE: `data: {"event_type": "...", ...}\n\n`
// with text deltas in `event_type: "text-generation"` (`text` field), tool
// invocations on `event_type: "tool-calls-generation"` (full `tool_calls`
// array) — Cohere does not stream tool args char-by-char like Anthropic
// does, so we just buffer until we see the generation event — and the
// terminal event `event_type: "stream-end"` carrying `finish_reason` and
// `response.meta.tokens.{input_tokens,output_tokens}`.

// cohereToolCall is one tool invocation as Cohere represents it on both the
// request side (assistant `tool_calls` field in chat_history) and the
// response side (`tool_calls` field on the body / stream events).
type cohereToolCall struct {
	Name       string         `json:"name"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

// cohereToolOutput is one tool-result body. Cohere allows multiple outputs
// per call (e.g. paginated results); we always emit exactly one with the
// canonical text rendering of the genai.FunctionResponse.
type cohereToolOutput struct {
	Output string `json:"output,omitempty"`
}

// cohereToolResult pairs a tool invocation with its result(s) for the
// `tool_results` request slot (current turn) or for a TOOL-role
// chat_history entry (prior turns).
type cohereToolResult struct {
	Call    cohereToolCall     `json:"call"`
	Outputs []cohereToolOutput `json:"outputs"`
}

// cohereChatTurn is one prior-turn entry. Cohere uses uppercase roles.
// ToolCalls is populated on CHATBOT turns that ended with a tool invocation;
// ToolResults is populated on TOOL turns that report the result of a prior
// CHATBOT-emitted call.
type cohereChatTurn struct {
	Role        string             `json:"role"` // "USER" | "CHATBOT" | "SYSTEM" | "TOOL"
	Message     string             `json:"message,omitempty"`
	ToolCalls   []cohereToolCall   `json:"tool_calls,omitempty"`
	ToolResults []cohereToolResult `json:"tool_results,omitempty"`
}

// cohereParamDef is one entry in a Cohere tool's `parameter_definitions` map.
type cohereParamDef struct {
	Description string `json:"description,omitempty"`
	Type        string `json:"type"`               // "str" | "int" | "float" | "bool" | "list" | "dict"
	Required    bool   `json:"required,omitempty"` // only marshal when true
}

// cohereToolDef is one entry in the request's `tools` array.
type cohereToolDef struct {
	Name                 string                    `json:"name"`
	Description          string                    `json:"description,omitempty"`
	ParameterDefinitions map[string]cohereParamDef `json:"parameter_definitions,omitempty"`
}

// cohereRequest is the body posted to publishers/cohere/models/<id>:rawPredict
// (or :streamRawPredict).
type cohereRequest struct {
	Message       string             `json:"message"`
	ChatHistory   []cohereChatTurn   `json:"chat_history,omitempty"`
	Preamble      string             `json:"preamble,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	MaxTokens     int32              `json:"max_tokens,omitempty"`
	Temperature   *float32           `json:"temperature,omitempty"`
	TopP          *float32           `json:"p,omitempty"`
	TopK          *int32             `json:"k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools         []cohereToolDef    `json:"tools,omitempty"`
	ToolResults   []cohereToolResult `json:"tool_results,omitempty"`
}

// translateGenaiToolsToCohere converts the gateway-internal []*genai.Tool list
// to Cohere's `tools` request array.
//
// Cohere's parameter_definitions schema is a simple flat map: each parameter
// has a description, a type (one of "str"|"int"|"float"|"bool"|"list"|"dict"),
// and a required flag. We extract these from the JSON Schema object on each
// FunctionDeclaration. Unknown / unsupported schema constructs degrade to
// "str" rather than aborting — Cohere will still accept the call and the
// model will figure it out from the description.
func translateGenaiToolsToCohere(in []*genai.Tool) []cohereToolDef {
	var out []cohereToolDef
	for _, t := range in {
		if t == nil {
			continue
		}
		for _, fd := range t.FunctionDeclarations {
			if fd == nil || fd.Name == "" {
				continue
			}
			td := cohereToolDef{
				Name:        fd.Name,
				Description: fd.Description,
			}
			schemaRaw, err := functionDeclarationSchemaToRaw(fd)
			if err == nil && len(schemaRaw) > 0 {
				td.ParameterDefinitions = jsonSchemaToCohereParams(schemaRaw)
			}
			out = append(out, td)
		}
	}
	return out
}

// jsonSchemaToCohereParams extracts Cohere's flat parameter_definitions
// representation from a JSON-Schema-shaped blob (the universal form we get
// from FunctionDeclaration.ParametersJsonSchema). Best-effort: we read
// .properties for the parameter map and .required for the required flag.
// Anything more nuanced (oneOf, refs, nested objects) is folded into
// type:"dict" so the model still sees the parameter name and description.
func jsonSchemaToCohereParams(raw json.RawMessage) map[string]cohereParamDef {
	var schema struct {
		Properties map[string]struct {
			Type        any    `json:"type"` // string OR []string (for nullable unions)
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	if len(schema.Properties) == 0 {
		return nil
	}
	requiredSet := make(map[string]bool, len(schema.Required))
	for _, r := range schema.Required {
		requiredSet[r] = true
	}
	out := make(map[string]cohereParamDef, len(schema.Properties))
	for name, p := range schema.Properties {
		out[name] = cohereParamDef{
			Description: p.Description,
			Type:        jsonSchemaTypeToCohere(p.Type),
			Required:    requiredSet[name],
		}
	}
	return out
}

// jsonSchemaTypeToCohere maps a JSON Schema type (which may be a single
// string, a list of strings for nullable unions, or absent entirely) to
// Cohere's restricted type vocabulary.
func jsonSchemaTypeToCohere(v any) string {
	pickOne := func(s string) string {
		switch s {
		case "string":
			return "str"
		case "integer":
			return "int"
		case "number":
			return "float"
		case "boolean":
			return "bool"
		case "array":
			return "list"
		case "object":
			return "dict"
		default:
			return "str"
		}
	}
	switch t := v.(type) {
	case string:
		return pickOne(t)
	case []any:
		// JSON Schema nullable union: pick the first non-"null" entry.
		for _, x := range t {
			if s, ok := x.(string); ok && s != "null" {
				return pickOne(s)
			}
		}
	}
	return "str"
}

// genaiPartsToCohereChatTurn converts a single genai.Content's parts into the
// Cohere chat_history representation for that turn. Returns the resulting
// turn plus a boolean indicating whether the turn carried any payload at all
// (skip-if-false to avoid emitting empty entries).
//
// Multiple FunctionCall parts in one assistant turn become multiple entries
// in the ToolCalls slice. Multiple FunctionResponse parts get folded into a
// single TOOL turn carrying multiple tool_results entries — Cohere prefers
// that grouping.
//
// Multimodal: Cohere's Vertex MaaS endpoint does NOT accept image inputs
// today (Command-A-Vision is direct-API-only). The handler-layer
// CheckVisionSupport gate rejects image-bearing requests for Cohere before
// they reach us, but as a defense-in-depth measure InlineData parts are
// silently dropped here — they would only generate confusing 400 errors
// from the upstream that we can't translate cleanly.
func genaiPartsToCohereChatTurn(role string, parts []*genai.Part) (turn cohereChatTurn, ok bool) {
	turn.Role = role
	var textBuf strings.Builder
	for _, p := range parts {
		if p == nil {
			continue
		}
		switch {
		case p.FunctionCall != nil:
			turn.ToolCalls = append(turn.ToolCalls, cohereToolCall{
				Name:       p.FunctionCall.Name,
				Parameters: p.FunctionCall.Args,
			})
		case p.FunctionResponse != nil:
			turn.ToolResults = append(turn.ToolResults, cohereToolResult{
				Call: cohereToolCall{Name: p.FunctionResponse.Name},
				Outputs: []cohereToolOutput{{
					Output: FunctionResponseToText(p.FunctionResponse),
				}},
			})
		case p.InlineData != nil:
			// Defense in depth — see function doc comment. Should be
			// unreachable via the handler path because CheckVisionSupport
			// gates this case.
			continue
		case p.Text != "":
			textBuf.WriteString(p.Text)
		}
	}
	turn.Message = textBuf.String()
	ok = turn.Message != "" || len(turn.ToolCalls) > 0 || len(turn.ToolResults) > 0
	// If we have ONLY tool_results, the role MUST be TOOL — promote here so
	// callers don't have to think about it.
	if len(turn.ToolResults) > 0 && turn.Message == "" && len(turn.ToolCalls) == 0 {
		turn.Role = "TOOL"
	}
	return turn, ok
}

// buildCohereRequest splits the conversation into history + final user
// message and assembles the Cohere body. Tool definitions, prior tool calls
// (assistant turns) and prior tool results (tool turns) are translated into
// Cohere's native shape so multi-turn tool conversations survive the round
// trip.
//
// Special-case for the LAST turn: when it is a TOOL turn (the client just
// supplied tool_result(s) and expects the model to keep going), we surface
// the tool results via the request-level `tool_results` slot — Cohere's API
// requires that for the current turn rather than re-using chat_history.
func buildCohereRequest(systemPrompt string, contents []*genai.Content, opts *pipeline.GenerationOptions, stream bool) cohereRequest {
	req := cohereRequest{
		Preamble: strings.TrimSpace(systemPrompt),
		Stream:   stream,
	}
	if opts != nil {
		req.Temperature = opts.Temperature
		req.TopP = opts.TopP
		req.TopK = opts.TopK
		req.StopSequences = opts.Stop
		if opts.MaxTokens != nil && *opts.MaxTokens > 0 {
			req.MaxTokens = *opts.MaxTokens
		}
		// Tool calling. Cohere honors `tool_choice` only implicitly via the
		// presence/absence of `tools`; there's no equivalent of Anthropic's
		// "any" or specific-tool pinning, so we ignore opts.ToolConfig
		// beyond the "none" suppression case.
		var suppressTools bool
		if opts.ToolConfig != nil && opts.ToolConfig.FunctionCallingConfig != nil &&
			opts.ToolConfig.FunctionCallingConfig.Mode == genai.FunctionCallingConfigModeNone {
			suppressTools = true
		}
		if !suppressTools {
			req.Tools = translateGenaiToolsToCohere(opts.Tools)
		}
	}

	// Flatten each genai.Content into one Cohere turn. We process the
	// sequence into a flat list first so we can identify the last user/tool
	// turn to split out as `message` / `tool_results`.
	var flat []cohereChatTurn
	for _, c := range contents {
		if c == nil {
			continue
		}
		role := "USER"
		if c.Role == genai.RoleModel || c.Role == "assistant" {
			role = "CHATBOT"
		}
		turn, ok := genaiPartsToCohereChatTurn(role, c.Parts)
		if !ok {
			continue
		}
		flat = append(flat, turn)
	}

	// Decide the "current turn" handling:
	//   - last turn is USER with text only → strip into `message`
	//   - last turn is TOOL (ONLY tool_results) → strip into `tool_results`
	//   - otherwise leave it in history and emit an empty `message` (Cohere
	//     accepts this and will continue from the last assistant turn)
	if n := len(flat); n > 0 {
		last := flat[n-1]
		switch {
		case last.Role == "USER" && len(last.ToolCalls) == 0 && len(last.ToolResults) == 0:
			req.Message = last.Message
			flat = flat[:n-1]
		case last.Role == "TOOL" && len(last.ToolResults) > 0:
			req.ToolResults = last.ToolResults
			flat = flat[:n-1]
		}
	}
	req.ChatHistory = flat
	return req
}

// cohereResponseToolCall is one tool call as returned by Cohere — same shape
// as the request side but kept as a separate type so the response decoder
// doesn't reuse a request type with json:"-" tags.
type cohereResponseToolCall = cohereToolCall

// cohereResponse is the non-streaming chat response shape.
type cohereResponse struct {
	Text         string                   `json:"text"`
	FinishReason string                   `json:"finish_reason"`
	ToolCalls    []cohereResponseToolCall `json:"tool_calls"`
	Meta         struct {
		Tokens struct {
			InputTokens  int32 `json:"input_tokens"`
			OutputTokens int32 `json:"output_tokens"`
		} `json:"tokens"`
	} `json:"meta"`
}

// cohereStreamEvent is the union shape of `event_type`-tagged stream events.
// Tool invocations arrive on "tool-calls-generation" with the full ToolCalls
// array populated — Cohere does not stream args char-by-char.
type cohereStreamEvent struct {
	EventType    string                   `json:"event_type"`
	Text         string                   `json:"text"`          // text-generation
	ToolCalls    []cohereResponseToolCall `json:"tool_calls"`    // tool-calls-generation
	FinishReason string                   `json:"finish_reason"` // stream-end
	Response     struct {
		Meta struct {
			Tokens struct {
				InputTokens  int32 `json:"input_tokens"`
				OutputTokens int32 `json:"output_tokens"`
			} `json:"tokens"`
		} `json:"meta"`
	} `json:"response"`
}

// mapCohereFinishReason converts Cohere's vocabulary into genai's. We need
// to look at the tool_calls presence at the call site to upgrade COMPLETE
// to FinishReasonToolCalls when the model actually emitted a tool call —
// Cohere does NOT have a dedicated tool-call finish reason.
func mapCohereFinishReason(r string) genai.FinishReason {
	switch strings.ToUpper(r) {
	case "COMPLETE", "STOP_SEQUENCE":
		return genai.FinishReasonStop
	case "MAX_TOKENS":
		return genai.FinishReasonMaxTokens
	case "ERROR_TOXIC":
		return genai.FinishReasonSafety
	case "":
		return ""
	default:
		return genai.FinishReasonOther
	}
}

// cohereToolCallsToGenaiParts converts Cohere-shaped tool calls into the
// genai Part list the rest of the gateway consumes. Cohere doesn't supply
// per-call IDs (their tool-result envelope keys on tool name) so we leave
// ID empty — the OpenAI surface will synthesize one downstream when needed.
func cohereToolCallsToGenaiParts(calls []cohereResponseToolCall) []*genai.Part {
	var parts []*genai.Part
	for _, tc := range calls {
		parts = append(parts, &genai.Part{
			FunctionCall: &genai.FunctionCall{
				Name: tc.Name,
				Args: tc.Parameters,
			},
		})
	}
	return parts
}

// cohereGenerate executes a non-streaming Cohere chat call and adapts the
// response into a *genai.GenerateContentResponse.
func (vc *VertexClient) cohereGenerate(ctx context.Context, modelID, systemPrompt string, contents []*genai.Content, opts *pipeline.GenerationOptions) (*genai.GenerateContentResponse, error) {
	body := buildCohereRequest(systemPrompt, contents, opts, false)
	DebugLogPayload(ctx, "upstream_request", body)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("cohere: marshal body: %w", err)
	}

	url := vc.publisherEndpoint("cohere", modelID, "rawPredict")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("cohere: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if vc.projectID != "" {
		req.Header.Set("x-goog-user-project", vc.projectID)
	}

	resp, err := vc.streamHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cohere: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("cohere: http %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	var cresp cohereResponse
	if err := json.NewDecoder(resp.Body).Decode(&cresp); err != nil {
		return nil, fmt.Errorf("cohere: decode: %w", err)
	}
	DebugLogPayload(ctx, "upstream_response", cresp)

	// Build the candidate Parts: text first (if any), then tool calls. If
	// the model emitted tool calls we upgrade the finish reason regardless
	// of what Cohere reported (it always reports COMPLETE).
	var parts []*genai.Part
	if cresp.Text != "" {
		parts = append(parts, &genai.Part{Text: cresp.Text})
	}
	parts = append(parts, cohereToolCallsToGenaiParts(cresp.ToolCalls)...)
	if len(parts) == 0 {
		parts = []*genai.Part{{Text: ""}}
	}
	finish := mapCohereFinishReason(cresp.FinishReason)
	if len(cresp.ToolCalls) > 0 {
		finish = FinishReasonToolCalls
	}

	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: parts,
			},
			FinishReason: finish,
		}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     cresp.Meta.Tokens.InputTokens,
			CandidatesTokenCount: cresp.Meta.Tokens.OutputTokens,
			TotalTokenCount:      cresp.Meta.Tokens.InputTokens + cresp.Meta.Tokens.OutputTokens,
		},
	}, nil
}

// cohereGenerateStream streams a Cohere chat call as `text-generation` SSE
// events, yielding per-chunk *genai.GenerateContentResponse values, plus a
// final synthetic chunk with FinishReason + UsageMetadata. Tool calls are
// emitted as a single FunctionCall chunk per call when the
// `tool-calls-generation` event arrives (Cohere does not stream tool args
// incrementally — that event carries the full args object).
func (vc *VertexClient) cohereGenerateStream(ctx context.Context, modelID, systemPrompt string, contents []*genai.Content, opts *pipeline.GenerationOptions) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		body := buildCohereRequest(systemPrompt, contents, opts, true)
		DebugLogPayload(ctx, "upstream_request", body)
		payload, err := json.Marshal(body)
		if err != nil {
			yield(nil, fmt.Errorf("cohere: marshal body: %w", err))
			return
		}

		url := vc.publisherEndpoint("cohere", modelID, "streamRawPredict")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			yield(nil, fmt.Errorf("cohere: build request: %w", err))
			return
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Accept", "text/event-stream")
		if vc.projectID != "" {
			req.Header.Set("x-goog-user-project", vc.projectID)
		}

		resp, err := vc.streamHTTP.Do(req)
		if err != nil {
			yield(nil, fmt.Errorf("cohere: http: %w", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			yield(nil, fmt.Errorf("cohere: http %d: %s",
				resp.StatusCode, strings.TrimSpace(string(errBody))))
			return
		}

		var (
			promptTokens int32
			outputTokens int32
			finishR      string
			sawToolCall  bool
		)

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
			// Cohere sometimes ships JSONL without `data:` framing — accept
			// either form.
			data := line
			if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
			if data == "" || data == "[DONE]" {
				continue
			}
			DebugLogPayload(ctx, "upstream_response_chunk", json.RawMessage(data))

			var ev cohereStreamEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			switch ev.EventType {
			case "text-generation":
				if ev.Text == "" {
					continue
				}
				chunk := &genai.GenerateContentResponse{
					Candidates: []*genai.Candidate{{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: []*genai.Part{{Text: ev.Text}},
						},
					}},
				}
				if !yield(chunk, nil) {
					return
				}
			case "tool-calls-generation":
				if len(ev.ToolCalls) == 0 {
					continue
				}
				sawToolCall = true
				chunk := &genai.GenerateContentResponse{
					Candidates: []*genai.Candidate{{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: cohereToolCallsToGenaiParts(ev.ToolCalls),
						},
					}},
				}
				if !yield(chunk, nil) {
					return
				}
			case "stream-end":
				finishR = ev.FinishReason
				if ev.Response.Meta.Tokens.InputTokens > 0 {
					promptTokens = ev.Response.Meta.Tokens.InputTokens
				}
				if ev.Response.Meta.Tokens.OutputTokens > 0 {
					outputTokens = ev.Response.Meta.Tokens.OutputTokens
				}
			}
		}
		if err := scanner.Err(); err != nil && err != io.EOF {
			yield(nil, fmt.Errorf("cohere: read stream: %w", err))
			return
		}

		finish := mapCohereFinishReason(finishR)
		if sawToolCall {
			finish = FinishReasonToolCalls
		}
		final := &genai.GenerateContentResponse{
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
				TotalTokenCount:      promptTokens + outputTokens,
			},
		}
		yield(final, nil)
	}
}
