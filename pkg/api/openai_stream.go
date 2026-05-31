package api

import (
	"go.f0o.dev/cline-vertex-gw/pkg/pipeline"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.f0o.dev/cline-vertex-gw/pkg/provider"
	"google.golang.org/genai"
)

// openaiChatNonStream handles the non-streaming branch of /v1/chat/completions.
//
// On success it emits a single OAIChatResponse JSON body. On error it writes
// an OpenAI-style error envelope with an appropriate HTTP status.
//
// Tool calling: FunctionCall parts in the response are extracted into the
// assistant message's `tool_calls` array; remaining text parts are
// concatenated into `content`. The finish reason is promoted to
// "tool_calls" when tool calls are present even if the underlying upstream
// reported a generic "stop".
func (h *APIHandler) openaiChatNonStream(
	ctx context.Context,
	w http.ResponseWriter,
	rl *reqLogger,
	req *OAIChatRequest,
	completionID string,
	created int64,
	systemPrompt string,
	contents []*genai.Content,
	genOpts *pipeline.GenerationOptions,
) {
	resp, metrics, err := h.runNonStreamWithRetry(
		ctx, rl, req.Model, systemPrompt, contents, genOpts)
	if err != nil {
		rl.Errorf("non-stream failed class=%s err=%v", classifyError(err), err)
		status, errType := httpStatusForUpstreamError(err)
		writeOAIError(w, status, errType,
			fmt.Sprintf("Error generating content: %v", err), "")
		return
	}

	var fullContent strings.Builder
	var toolCalls []OAIToolCall
	if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
		for _, part := range resp.Candidates[0].Content.Parts {
			if part.FunctionCall != nil {
				toolCalls = append(toolCalls, oaiToolCallFromGenai(part))
				continue
			}
			fullContent.WriteString(part.Text)
		}
	}
	contentStr := fullContent.String()
	pub, _ := provider.ParsePublisher(req.Model)
	if pub == "google" {
		unescaper := NewEntityUnescaper()
		contentStr = unescaper.Process(contentStr) + unescaper.Flush()
	}
	finish := finishReasonOAI(metrics.finishReason)
	if len(toolCalls) > 0 && finish != "tool_calls" {
		// Defensive upgrade: if the upstream reported "stop" but emitted
		// tool calls, the client expects to see finish_reason: tool_calls.
		finish = "tool_calls"
	}

	out := OAIChatResponse{
		ID:      completionID,
		Object:  "chat.completion",
		Created: created,
		Model:   req.Model,
		Choices: []OAIChatChoice{{
			Index: 0,
			Message: OAIResponseMessage{
				Role:      "assistant",
				Content:   contentStr,
				ToolCalls: toolCalls,
			},
			FinishReason: finish,
		}},
		Usage: OAIUsage{
			PromptTokens:     metrics.promptTokens,
			CompletionTokens: metrics.completionTokens,
			TotalTokens:      metrics.promptTokens + metrics.completionTokens,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	provider.DebugLogPayload(ctx, "outbound_response", out)
	if err := json.NewEncoder(w).Encode(out); err != nil {
		rl.Errorf("encode response: %v", err)
	}
	logCompletionForModel(ctx, rl, "v1-chat", req.Model, metrics)
}

// openaiChatStream handles the streaming branch of /v1/chat/completions.
//
// Emits OpenAI-compatible Server-Sent Events:
//
//	data: {"id":"chatcmpl-...","object":"chat.completion.chunk", ...}
//
//	data: {... final chunk with finish_reason and usage ...}
//
//	data: [DONE]
//
// The first chunk carries a "role":"assistant" delta with no content
// (matching the OpenAI reference implementation). Subsequent chunks carry
// content deltas (for text) OR tool_calls deltas (for tool invocations).
// Tool-call deltas use OpenAI's standard accumulating shape: the first
// chunk for a given index carries `id`+`type`+`function.name`+initial
// `function.arguments`; subsequent chunks for the same index would append
// to `function.arguments`. Since publisher adapters emit fully-assembled
// FunctionCall units (not byte-streamed args), we always emit each tool
// call as a single chunk with the complete argument JSON in one shot —
// this matches OpenAI's wire shape exactly and is what clients expect on
// the receiving end.
//
// The penultimate chunk carries finish_reason; the final usage chunk
// is sent separately so clients that key on finish_reason being non-null
// work correctly. Stream is then terminated with the `[DONE]` sentinel.
func (h *APIHandler) openaiChatStream(
	ctx context.Context,
	w http.ResponseWriter,
	rl *reqLogger,
	req *OAIChatRequest,
	completionID string,
	created int64,
	systemPrompt string,
	contents []*genai.Content,
	genOpts *pipeline.GenerationOptions,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		rl.Errorf("streaming unsupported by responseWriter")
		writeOAIError(w, http.StatusInternalServerError, "server_error",
			"Streaming unsupported", "")
		return
	}

	// SSE headers. Setting these before the first Flush is critical; once
	// bytes are written the status line and headers are locked in.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx/loadbalancer buffering
	w.WriteHeader(http.StatusOK)

	// Track whether we've successfully emitted at least one SSE event so we
	// know to surface mid-stream errors over SSE rather than as a fresh HTTP
	// error (which would be ignored by the client).
	emittedAny := false
	// toolCallIndex is the monotonically-increasing index we assign to each
	// tool call we emit (per OpenAI's wire spec — the index lets clients
	// keep parallel tool calls separate). We assign sequentially since
	// every upstream emits parallel calls in order.
	toolCallIndex := 0
	// sawToolCall tracks whether any tool_calls delta was emitted; the
	// final chunk's finish_reason is upgraded to "tool_calls" when so.
	sawToolCall := false

	// emitFirstDelta emits the initial role-only chunk that OpenAI's reference
	// client expects before any content deltas.
	emitFirstDelta := func() error {
		chunk := OAIChatStreamResponse{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   req.Model,
			Choices: []OAIChatChoiceStream{{
				Index:        0,
				Delta:        OAIStreamDelta{Role: "assistant"},
				FinishReason: nil,
			}},
		}
		if err := writeSSEData(ctx, w, chunk); err != nil {
			return err
		}
		flusher.Flush()
		emittedAny = true
		return nil
	}

	// emitTextDelta sends a content delta chunk.
	emitTextDelta := func(text string) error {
		if !emittedAny {
			if err := emitFirstDelta(); err != nil {
				return err
			}
		}
		chunk := OAIChatStreamResponse{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   req.Model,
			Choices: []OAIChatChoiceStream{{
				Index:        0,
				Delta:        OAIStreamDelta{Content: text},
				FinishReason: nil,
			}},
		}
		if err := writeSSEData(ctx, w, chunk); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// emitToolCallDelta sends a single tool_calls delta chunk carrying one
	// fully-assembled tool call. Per OpenAI's wire spec, each call has a
	// unique index, a stable id, and the arguments are a JSON-encoded string.
	emitToolCallDelta := func(part *genai.Part) error {
		if !emittedAny {
			if err := emitFirstDelta(); err != nil {
				return err
			}
		}
		fc := part.FunctionCall
		argsJSON, _ := provider.MarshalToolArgs(fc.Args)
		id := fc.ID
		if id == "" {
			id = newToolCallID()
		}
		if len(part.ThoughtSignature) > 0 {
			id = id + "|" + base64.URLEncoding.EncodeToString(part.ThoughtSignature)
		}
		tcd := OAIStreamToolCallDelta{
			Index: toolCallIndex,
			ID:    id,
			Type:  "function",
		}
		tcd.Function.Name = fc.Name
		tcd.Function.Arguments = argsJSON
		toolCallIndex++
		sawToolCall = true

		chunk := OAIChatStreamResponse{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   req.Model,
			Choices: []OAIChatChoiceStream{{
				Index:        0,
				Delta:        OAIStreamDelta{ToolCalls: []OAIStreamToolCallDelta{tcd}},
				FinishReason: nil,
			}},
		}
		if err := writeSSEData(ctx, w, chunk); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	var unescaper *EntityUnescaper
	pubStream, _ := provider.ParsePublisher(req.Model)
	if pubStream == "google" {
		unescaper = NewEntityUnescaper()
	}

	onDelta := func(d StreamDelta) error {
		if d.Part != nil && d.Part.FunctionCall != nil {
			return emitToolCallDelta(d.Part)
		}
		if d.Text != "" {
			text := d.Text
			if unescaper != nil {
				text = unescaper.Process(text)
			}
			if text == "" {
				return nil
			}
			return emitTextDelta(text)
		}
		return nil
	}

	metrics, err := h.runStreamWithRetry(ctx, rl, req.Model, systemPrompt, contents, genOpts, onDelta)

	if err != nil {
		rl.Errorf("stream failed class=%s err=%v", classifyError(err), err)
		if !emittedAny {
			// Headers are already sent (SSE-shaped). Emit the error as an
			// SSE event with the OpenAI error envelope, then close the stream.
			errEvent := OAIErrorResponse{
				Error: OAIErrorBody{
					Message: fmt.Sprintf("Vertex AI stream error: %v", err),
					Type:    "upstream_error",
				},
			}
			_ = writeSSEData(ctx, w, errEvent)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}
		// Mid-stream failure: best-effort error event then terminate.
		errEvent := OAIErrorResponse{
			Error: OAIErrorBody{
				Message: fmt.Sprintf("Vertex AI stream error mid-completion: %v", err),
				Type:    "upstream_error",
			},
		}
		_ = writeSSEData(ctx, w, errEvent)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
		return
	}

	if unescaper != nil {
		leftover := unescaper.Flush()
		if leftover != "" {
			if err := emitTextDelta(leftover); err != nil {
				rl.Errorf("emit leftover: %v", err)
			}
		}
	}

	// Ensure clients always see at least one event even for empty completions.
	if !emittedAny {
		if err := emitFirstDelta(); err != nil {
			rl.Errorf("emit first delta: %v", err)
			return
		}
	}

	// Final delta chunk: empty content + finish_reason set.
	finish := finishReasonOAI(metrics.finishReason)
	if sawToolCall && finish != "tool_calls" {
		// Defensive upgrade for upstreams that reported "stop" despite
		// emitting tool calls (some MaaS publishers do this).
		finish = "tool_calls"
	}
	finalChunk := OAIChatStreamResponse{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   req.Model,
		Choices: []OAIChatChoiceStream{{
			Index:        0,
			Delta:        OAIStreamDelta{},
			FinishReason: &finish,
		}},
		Usage: &OAIUsage{
			PromptTokens:     metrics.promptTokens,
			CompletionTokens: metrics.completionTokens,
			TotalTokens:      metrics.promptTokens + metrics.completionTokens,
		},
	}
	if err := writeSSEData(ctx, w, finalChunk); err != nil {
		rl.Errorf("encode final chunk: %v", err)
	}
	// Terminator sentinel.
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
	logCompletionForModel(ctx, rl, "v1-chat-stream", req.Model, metrics)
}

// writeSSEData JSON-encodes v and writes it as a single SSE `data: ...` event.
// The encoded JSON is guaranteed to be on a single line (no embedded LFs since
// encoding/json does not insert newlines into strings, but it does append a
// trailing '\n' from Encode which we replace), so the SSE framing remains
// `data: <json>\n\n`.
func writeSSEData(ctx context.Context, w http.ResponseWriter, v any) error {
	provider.DebugLogPayload(ctx, "outbound_response_chunk", v)
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal sse: %w", err)
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n\n"))
	return err
}

// httpStatusForUpstreamError maps an upstream error to a reasonable HTTP
// status and OpenAI-style error type for the response envelope.
func httpStatusForUpstreamError(err error) (status int, errType string) {
	switch classifyError(err) {
	case "rate-limited":
		return http.StatusTooManyRequests, "rate_limit_exceeded"
	case "auth":
		return http.StatusUnauthorized, "authentication_error"
	case "not-found":
		return http.StatusNotFound, "not_found_error"
	case "unavailable", "upstream-5xx", "network", "overloaded":
		return http.StatusBadGateway, "upstream_error"
	case "client-canceled":
		// Return 499-equivalent. OpenAI doesn't have a canonical mapping;
		// use 408 (request timeout) which most SDKs treat as a transient
		// failure rather than a misconfiguration.
		return http.StatusRequestTimeout, "request_canceled"
	case "deadline-exceeded":
		return http.StatusGatewayTimeout, "timeout_error"
	default:
		return http.StatusBadGateway, "upstream_error"
	}
}
