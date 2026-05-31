package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.f0o.dev/cline-vertex-gw/provider"
	"google.golang.org/genai"
)

// OpenAI-compatible HTTP handlers.
//
// These reuse the same internal Vertex translation primitives as the Ollama
// handlers (runStreamWithRetry, runNonStreamWithRetry) so upstream behavior
// is identical; only the request decoding and response encoding differ.
//
// Endpoints implemented:
//   - GET  /v1/models                — list available models
//   - POST /v1/chat/completions      — chat completion (SSE stream or JSON)
//
// Authentication: the gateway's shared bearer token middleware applies to
// /v1/* exactly like it does to /api/*. OpenAI clients typically inject this
// via the `Authorization: Bearer <api-key>` header automatically.

// newCompletionID returns a stable OpenAI-style id ("chatcmpl-<hex>") so
// downstream tooling that keys on the field has something to anchor on.
func newCompletionID() string {
	return "chatcmpl-" + newReqID()
}

// OpenAIModelsHandler implements GET /v1/models in the OpenAI shape, sourced
// from the same Vertex discovery as /api/tags.
func (h *APIHandler) OpenAIModelsHandler(w http.ResponseWriter, r *http.Request) {
	rl := newReqLogger("v1-models")
	if r.Method != http.MethodGet {
		writeOAIError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"Only GET is allowed on this endpoint", "")
		return
	}
	if h.Vertex == nil {
		rl.Logf("aborting: Vertex client not configured")
		writeOAIError(w, http.StatusInternalServerError, "server_error",
			"Vertex AI client not configured", "")
		return
	}

	ctx := r.Context()
	vertexModels, err := h.Vertex.ListModelsCached(ctx)
	if err != nil {
		rl.Logf("error fetching models: %v", err)
		writeOAIError(w, http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("Error fetching models from Vertex AI: %v", err), "")
		return
	}

	created := time.Now().Unix()
	out := make([]OAIModel, 0, len(vertexModels))
	for _, m := range vertexModels {
		baseName := m.Name
		if idx := strings.LastIndex(baseName, "/"); idx >= 0 {
			baseName = baseName[idx+1:]
		}
		publisher, _ := provider.ParsePublisher(baseName)
		out = append(out, OAIModel{
			ID:      baseName,
			Object:  "model",
			Created: created,
			OwnedBy: publisher,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(OAIModelsResponse{
		Object: "list",
		Data:   out,
	}); err != nil {
		rl.Logf("encode response: %v", err)
		return
	}
	rl.Logf("served %d models in %v", len(out), rl.Elapsed())
}

// buildContentsOAI converts OpenAI messages to Vertex Contents + a hoisted
// system prompt. Mirrors buildContents() for the Ollama shape.
//
// Tool calling: assistant turns carrying `tool_calls` are preserved as
// FunctionCall parts on a "model"-role Content (NOT skipped because their
// text content is empty — that was the bug). `role:"tool"` messages become
// FunctionResponse parts on a "user"-role Content, which every publisher
// adapter translates into its native tool-result shape downstream.
//
// Multimodal: a message whose `content` is a parts array containing
// `image_url` entries with `data:` URLs is decoded into genai.Part
// InlineData parts in the order the client sent them. The
// `GW_MAX_IMAGE_BYTES_PER_REQUEST` cap is enforced across all decoded
// image bytes in the request. The legacy `role:"function"` shape (pre-2023
// OpenAI) is treated like `role:"tool"` for backward compatibility.
//
// Returns a clear error (which the handler maps to HTTP 400) when an image
// part fails to decode or the request exceeds the per-request image budget.
// Decoding text-only or empty messages always succeeds.
func buildContentsOAI(messages []OAIChatMessage) (contents []*genai.Content, systemPrompt string, err error) {
	var lastRole string
	var currentParts []*genai.Part
	totalImageBytesSeen := 0

	flush := func() {
		if lastRole != "" && len(currentParts) > 0 {
			contents = append(contents, &genai.Content{Role: lastRole, Parts: currentParts})
		}
	}

	for msgIdx, msg := range messages {
		role := strings.ToLower(msg.Role)

		// Decode the polymorphic content slot. ContentParts handles the
		// bare-string and parts-array forms (including image_url parts).
		// For system/developer + tool/function roles we only ever want
		// the flattened text view; for user/assistant we keep the parts.
		mparts, perr := msg.ContentParts()
		if perr != nil {
			return nil, "", fmt.Errorf("message %d: %w", msgIdx, perr)
		}
		text := concatText(mparts)

		if role == "system" || role == "developer" {
			// "developer" is the o-series replacement for "system". Images
			// on a system message are silently dropped — no upstream
			// supports them as system context.
			if text != "" {
				systemPrompt += text + "\n"
			}
			continue
		}

		// Per-request image-bytes cap. Counted across ALL messages, not
		// just the current one, so a single oversized image plus several
		// smaller ones still trips it.
		totalImageBytesSeen += totalImageBytes(mparts)
		if totalImageBytesSeen > maxImageBytesPerRequest {
			return nil, "", fmt.Errorf("request image payload exceeds %d bytes (GW_MAX_IMAGE_BYTES_PER_REQUEST)",
				maxImageBytesPerRequest)
		}

		// Build the per-message parts list. mediaParts (text + image) go
		// first in their original order, then FunctionCall parts for
		// assistant tool_calls, then FunctionResponse parts for
		// tool/function messages.
		msgParts := mediaPartsToGenai(mparts)
		switch role {
		case "assistant":
			for _, tc := range msg.ToolCalls {
				if tc.Function.Name == "" {
					continue
				}
				msgParts = append(msgParts, oaiToolCallToGenaiPart(tc))
			}
		case "tool", "function":
			// Tool result. Name comes from msg.Name on `function` shape and
			// is conventionally present-but-not-required on `tool` shape;
			// we accept either. Tool results never carry images — we
			// deliberately discard any image parts on a tool message.
			toolName := msg.Name
			msgParts = []*genai.Part{toolResultPart(msg.ToolCallID, toolName, text)}
			// Tool results are conventionally sent as a "user" turn from
			// the model's perspective.
			role = "user"
		}
		if len(msgParts) == 0 {
			continue
		}
		vRole := provider.MapRole(role)
		if vRole == lastRole {
			currentParts = append(currentParts, msgParts...)
			continue
		}
		flush()
		lastRole = vRole
		currentParts = msgParts
	}
	flush()
	return contents, systemPrompt, nil
}

// genOptionsFromOAI converts an OpenAI chat request's tuning fields into the
// provider's GenerationOptions. Returns nil if no relevant fields are set so
// the upstream call uses publisher defaults.
//
// Tool calling: req.Tools and req.Functions (legacy) are translated into
// genai.Tool entries; req.ToolChoice is translated into genai.ToolConfig.
// When either is non-empty the returned options will be non-nil even if no
// sampling parameters were set, because tool calling itself is a settings
// change the publisher adapters need to see.
func genOptionsFromOAI(req *OAIChatRequest) *provider.GenerationOptions {
	if req == nil {
		return nil
	}
	hasAny := false
	out := &provider.GenerationOptions{}
	if req.Temperature != nil {
		out.Temperature = req.Temperature
		hasAny = true
	}
	if req.TopP != nil {
		out.TopP = req.TopP
		hasAny = true
	}
	if req.TopK != nil {
		out.TopK = req.TopK
		hasAny = true
	}
	if len(req.Stop) > 0 {
		out.Stop = req.Stop
		hasAny = true
	}
	// max_tokens / max_output_tokens (OpenAI o-series alias). Prefer the
	// non-alias if both are present.
	if req.MaxTokens != nil {
		out.MaxTokens = req.MaxTokens
		hasAny = true
	} else if req.MaxOutputTokens != nil {
		out.MaxTokens = req.MaxOutputTokens
		hasAny = true
	}
	if tools := translateOAIToolsToGenai(req.Tools, req.Functions); len(tools) > 0 {
		out.Tools = tools
		hasAny = true
	}
	if cfg := translateOAIToolChoiceToGenai(req.ToolChoice); cfg != nil {
		out.ToolConfig = cfg
		hasAny = true
	}
	if !hasAny {
		return nil
	}
	return out
}

// finishReasonOAI normalises a Vertex finish reason into the OpenAI-style
// terminal values that clients expect on the final chunk / response.
func finishReasonOAI(vertexReason string) string {
	switch strings.ToUpper(vertexReason) {
	case "", "STOP", "FINISH_REASON_STOP":
		return "stop"
	case "MAX_TOKENS", "FINISH_REASON_MAX_TOKENS":
		return "length"
	case "SAFETY", "FINISH_REASON_SAFETY":
		return "content_filter"
	case "RECITATION", "FINISH_REASON_RECITATION":
		return "content_filter"
	case "TOOL_USE", "TOOL_CALLS":
		return "tool_calls"
	default:
		return "stop"
	}
}

// writeOAIError emits an OpenAI-style error envelope. Safe to call only
// before any bytes have been streamed.
func writeOAIError(w http.ResponseWriter, status int, errType, msg, code string) {
	writeJSON(w, status, OAIErrorResponse{
		Error: OAIErrorBody{
			Message: msg,
			Type:    errType,
			Code:    code,
		},
	})
}

// OpenAIChatCompletionsHandler implements POST /v1/chat/completions.
//
// Supports both streaming (text/event-stream with the OpenAI `data: {...}\n\n`
// + `data: [DONE]\n\n` framing) and non-streaming JSON responses, gated by
// the `stream` field in the request body.
func (h *APIHandler) OpenAIChatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	rl := newReqLogger("v1-chat")
	ctx := r.Context()
	ctx = context.WithValue(ctx, provider.ContextKeyReqID, rl.id)
	ctx = context.WithValue(ctx, provider.ContextKeyRoute, rl.route)
	r = r.WithContext(ctx)

	if r.Method != http.MethodPost {
		writeOAIError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"Only POST is allowed on this endpoint", "")
		return
	}
	if h.Vertex == nil {
		rl.Logf("aborting: Vertex client not configured")
		writeOAIError(w, http.StatusInternalServerError, "server_error",
			"Vertex AI client not configured", "")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			rl.Logf("body exceeded max size: %v", err)
			writeOAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
				"Request body too large", "")
			return
		}
		rl.Logf("read body: %v", err)
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error",
			"Error reading request body", "")
		return
	}
	defer r.Body.Close()

	var req OAIChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		rl.Logf("parse body: %v", err)
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("Error parsing JSON body: %v", err), "")
		return
	}

	provider.DebugLogPayload(ctx, "inbound_request", req)

	if strings.TrimSpace(req.Model) == "" {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error",
			"Missing required field: model", "model")
		return
	}
	if len(req.Messages) == 0 {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error",
			"Missing required field: messages", "messages")
		return
	}

	rl.Logf("model=%s stream=%v messages=%d body=%dB",
		req.Model, req.Stream, len(req.Messages), len(body))

	contents, systemPrompt, bcerr := buildContentsOAI(req.Messages)
	if bcerr != nil {
		// Image-decode failures and per-request image-budget overruns land
		// here. Return 400 + a clear error envelope so the caller can fix
		// the request rather than silently dropping image context.
		rl.Logf("rejected: build contents: %v", bcerr)
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", bcerr.Error(), "messages")
		return
	}
	if len(contents) == 0 {
		rl.Logf("rejected: no valid messages")
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error",
			"No valid (non-system) messages provided", "messages")
		return
	}
	// Capability gate: reject image inputs on publisher/model combos that
	// don't support them. The Cohere adapter would silently produce
	// nonsense; some MaaS models will 400 mid-stream after we've already
	// committed headers. Catching it here gives a clean 400.
	if capErr := provider.CheckVisionSupport(req.Model, contents); capErr != nil {
		rl.Logf("rejected: vision capability: %v", capErr)
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", capErr.Error(), "model")
		return
	}
	genOpts := genOptionsFromOAI(&req)
	ctx = r.Context()

	completionID := newCompletionID()
	created := time.Now().Unix()

	if req.Stream {
		h.openaiChatStream(ctx, w, rl, &req, completionID, created, systemPrompt, contents, genOpts)
		return
	}
	h.openaiChatNonStream(ctx, w, rl, &req, completionID, created, systemPrompt, contents, genOpts)
}
