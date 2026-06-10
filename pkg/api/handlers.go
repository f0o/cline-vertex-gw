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

	"go.f0o.dev/cline-vertex-gw/pkg/provider"
	"google.golang.org/genai"
)

type APIHandler struct {
	Vertex *provider.VertexClient
}

// TagsHandler implements the /api/tags Ollama model discovery endpoint.
func (h *APIHandler) TagsHandler(w http.ResponseWriter, r *http.Request) {
	rl := newReqLogger("tags")
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.Vertex == nil {
		rl.Errorf("aborting: Vertex client not configured")
		http.Error(w, "Vertex AI client not configured", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	vertexModels, err := h.Vertex.ListModelsCached(ctx)
	if err != nil {
		rl.Errorf("error fetching models: %v", err)
		http.Error(w, "Error fetching models from Vertex AI", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	out := make([]TagModel, 0, len(vertexModels))
	for _, m := range vertexModels {
		baseName := m.Name
		if idx := strings.LastIndex(baseName, "/"); idx >= 0 {
			baseName = baseName[idx+1:]
		}
		family := familyFromName(baseName)
		out = append(out, TagModel{
			Name:       baseName,
			Model:      baseName,
			ModifiedAt: now,
			// Size is unknown for hosted models; report 0 instead of a fake value.
			Size:   0,
			Digest: "",
			Details: ModelDetails{
				Format:        "vertex",
				Family:        family,
				Families:      []string{family},
				ParameterSize: "",
			},
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(TagsResponse{Models: out}); err != nil {
		rl.Errorf("encode response: %v", err)
		return
	}
	rl.Logf("served %d models in %v", len(out), rl.Elapsed())
}

// ChatHandler implements the /api/chat Ollama chat completion endpoint.
func (h *APIHandler) ChatHandler(w http.ResponseWriter, r *http.Request) {
	rl := newReqLogger("chat")
	ctx := r.Context()
	ctx = context.WithValue(ctx, provider.ContextKeyReqID, rl.id)
	ctx = context.WithValue(ctx, provider.ContextKeyRoute, rl.route)
	r = r.WithContext(ctx)

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Vertex == nil {
		rl.Errorf("aborting: Vertex client not configured")
		http.Error(w, "Vertex AI client not configured", http.StatusInternalServerError)
		return
	}
	// Refresh the live pricing table in the background if its TTL has elapsed.
	// Non-blocking: never adds latency to this request.
	h.Vertex.MaybeRefreshPricing(ctx)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			rl.Logf("body exceeded max size: %v", err)
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		rl.Errorf("read body: %v", err)
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req ChatRequest

	if err := json.Unmarshal(body, &req); err != nil {
		rl.Errorf("parse body: %v", err)
		http.Error(w, "Error parsing JSON body", http.StatusBadRequest)
		return
	}

	provider.DebugLogPayload(ctx, "inbound_request", req)

	isStream := true
	if req.Stream != nil {
		isStream = *req.Stream
	}
	rl.Logf("model=%s stream=%v messages=%d body=%dB",
		req.Model, isStream, len(req.Messages), len(body))

	contents, systemPrompt, bcerr := buildContents(req.Messages)
	if bcerr != nil {
		// Image-decode failures and per-request image-budget overruns land
		// here. 400 with the decoded error message so the caller can fix
		// the request (e.g. shrink the image, use a supported format).
		rl.Errorf("rejected: build contents: %v", bcerr)
		http.Error(w, bcerr.Error(), http.StatusBadRequest)
		return
	}
	if len(contents) == 0 {
		rl.Errorf("rejected: no valid messages")
		http.Error(w, "No valid messages provided", http.StatusBadRequest)
		return
	}
	if capErr := provider.CheckVisionSupport(req.Model, contents); capErr != nil {
		rl.Errorf("rejected: vision capability: %v", capErr)
		http.Error(w, capErr.Error(), http.StatusBadRequest)
		return
	}
	genOpts := genOptionsFromAPI(req.Options, req.Tools)
	ctx = r.Context()

	if isStream {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, ok := w.(http.Flusher)
		if !ok {
			rl.Errorf("streaming unsupported by responseWriter")
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		enc := json.NewEncoder(w)
		// Collected tool calls (for the final Done message). Ollama's wire
		// format supports `message.tool_calls` on the terminal chunk only —
		// per-chunk tool deltas would require a custom Ollama extension
		// most clients don't speak. So we buffer them and emit on Done.
		var collectedToolCalls []ToolCall

		var unescaper *EntityUnescaper
		pub, _ := provider.ParsePublisher(req.Model)
		if pub == "google" {
			unescaper = NewEntityUnescaper()
		}

		onChunk := func(d StreamDelta) error {
			if d.Part != nil && d.Part.FunctionCall != nil {
				collectedToolCalls = append(collectedToolCalls,
					toolCallFromGenai(d.Part))
				return nil
			}
			if d.Text == "" {
				return nil
			}
			text := d.Text
			if unescaper != nil {
				text = unescaper.Process(text)
			}
			if text == "" {
				return nil
			}
			chunk := ChatResponse{
				Model:     req.Model,
				CreatedAt: time.Now(),
				Message:   Message{Role: "assistant", Content: text},
				Done:      false,
			}
			provider.DebugLogPayload(ctx, "outbound_response_chunk", chunk)
			err := enc.Encode(chunk)
			if err != nil {
				return err
			}
			flusher.Flush()
			return nil
		}

		metrics, err := h.runStreamWithRetry(ctx, rl, req.Model, systemPrompt, contents, genOpts, onChunk)
		if err != nil {
			// Try to surface the error via the stream if at all possible; if
			// the client already disconnected this is best-effort.
			rl.Errorf("stream failed class=%s err=%v", classifyError(err), err)
			_ = enc.Encode(map[string]string{"error": fmt.Sprintf("Vertex AI stream error: %v", err)})
			flusher.Flush()
			return
		}

		if unescaper != nil {
			leftover := unescaper.Flush()
			if leftover != "" {
				chunk := ChatResponse{
					Model:     req.Model,
					CreatedAt: time.Now(),
					Message:   Message{Role: "assistant", Content: leftover},
					Done:      false,
				}
				provider.DebugLogPayload(ctx, "outbound_response_chunk", chunk)
				_ = enc.Encode(chunk)
				flusher.Flush()
			}
		}

		total, load, promptDur, evalDur := metrics.finalize()
		done := ChatResponse{
			Model:              req.Model,
			CreatedAt:          time.Now(),
			Message:            Message{Role: "assistant", Content: "", ToolCalls: collectedToolCalls},
			Done:               true,
			DoneReason:         doneReason(metrics.finishReason),
			TotalDuration:      total,
			LoadDuration:       load,
			PromptEvalCount:    metrics.promptTokens,
			PromptEvalDuration: promptDur,
			EvalCount:          metrics.completionTokens,
			EvalDuration:       evalDur,
		}
		provider.DebugLogPayload(ctx, "outbound_response_chunk", done)
		if err := enc.Encode(done); err != nil {
			rl.Errorf("encode done: %v", err)
		}
		flusher.Flush()
		logCompletionForModel(ctx, rl, "chat-stream", req.Model, metrics)
		return
	}

	// Non-streaming branch.
	w.Header().Set("Content-Type", "application/json")
	resp, metrics, err := h.runNonStreamWithRetry(ctx, rl, req.Model, systemPrompt, contents, genOpts)
	if err != nil {
		rl.Errorf("non-stream failed class=%s err=%v", classifyError(err), err)
		http.Error(w, fmt.Sprintf("Error generating content: %v", err), http.StatusBadGateway)
		return
	}

	var fullContent strings.Builder
	var nonStreamToolCalls []ToolCall
	if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
		for _, part := range resp.Candidates[0].Content.Parts {
			if part.FunctionCall != nil {
				nonStreamToolCalls = append(nonStreamToolCalls,
					toolCallFromGenai(part))
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
	total, load, promptDur, evalDur := metrics.finalize()
	chatResp := ChatResponse{
		Model:              req.Model,
		CreatedAt:          time.Now(),
		Message:            Message{Role: "assistant", Content: contentStr, ToolCalls: nonStreamToolCalls},
		Done:               true,
		DoneReason:         doneReason(metrics.finishReason),
		TotalDuration:      total,
		LoadDuration:       load,
		PromptEvalCount:    metrics.promptTokens,
		PromptEvalDuration: promptDur,
		EvalCount:          metrics.completionTokens,
		EvalDuration:       evalDur,
	}
	provider.DebugLogPayload(ctx, "outbound_response", chatResp)
	if err := json.NewEncoder(w).Encode(chatResp); err != nil {
		rl.Errorf("encode response: %v", err)
	}
	logCompletionForModel(ctx, rl, "chat", req.Model, metrics)
}

// GenerateHandler implements the /api/generate Ollama completion endpoint.
func (h *APIHandler) GenerateHandler(w http.ResponseWriter, r *http.Request) {
	rl := newReqLogger("generate")
	ctx := r.Context()
	ctx = context.WithValue(ctx, provider.ContextKeyReqID, rl.id)
	ctx = context.WithValue(ctx, provider.ContextKeyRoute, rl.route)
	r = r.WithContext(ctx)

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Vertex == nil {
		rl.Errorf("aborting: Vertex client not configured")
		http.Error(w, "Vertex AI client not configured", http.StatusInternalServerError)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			rl.Logf("body exceeded max size: %v", err)
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		rl.Errorf("read body: %v", err)
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req GenerateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		rl.Errorf("parse body: %v", err)
		http.Error(w, "Error parsing JSON body", http.StatusBadRequest)
		return
	}

	provider.DebugLogPayload(ctx, "inbound_request", req)

	isStream := true
	if req.Stream != nil {
		isStream = *req.Stream
	}
	rl.Logf("model=%s stream=%v prompt=%dB body=%dB",
		req.Model, isStream, len(req.Prompt), len(body))

	contents := []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: req.Prompt}},
	}}
	// /api/generate has no tools field in real Ollama; pass nil.
	genOpts := genOptionsFromAPI(req.Options, nil)
	ctx = r.Context()

	if isStream {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, ok := w.(http.Flusher)
		if !ok {
			rl.Errorf("streaming unsupported by responseWriter")
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		enc := json.NewEncoder(w)
		// The Ollama /api/generate wire shape has no slot for tool calls
		// (unlike /api/chat). FunctionCall deltas are silently dropped here
		// — clients that want tool calling against this gateway should use
		// /api/chat or /v1/chat/completions instead.
		var unescaper *EntityUnescaper
		pub, _ := provider.ParsePublisher(req.Model)
		if pub == "google" {
			unescaper = NewEntityUnescaper()
		}

		onChunk := func(d StreamDelta) error {
			if d.Part != nil && d.Part.FunctionCall != nil {
				return nil
			}
			if d.Text == "" {
				return nil
			}
			text := d.Text
			if unescaper != nil {
				text = unescaper.Process(text)
			}
			if text == "" {
				return nil
			}
			chunk := GenerateResponse{
				Model:     req.Model,
				CreatedAt: time.Now(),
				Response:  text,
				Done:      false,
			}
			provider.DebugLogPayload(ctx, "outbound_response_chunk", chunk)
			err := enc.Encode(chunk)
			if err != nil {
				return err
			}
			flusher.Flush()
			return nil
		}

		metrics, err := h.runStreamWithRetry(ctx, rl, req.Model, req.System, contents, genOpts, onChunk)
		if err != nil {
			rl.Errorf("stream failed class=%s err=%v", classifyError(err), err)
			_ = enc.Encode(map[string]string{"error": fmt.Sprintf("Vertex AI stream error: %v", err)})
			flusher.Flush()
			return
		}

		if unescaper != nil {
			leftover := unescaper.Flush()
			if leftover != "" {
				chunk := GenerateResponse{
					Model:     req.Model,
					CreatedAt: time.Now(),
					Response:  leftover,
					Done:      false,
				}
				provider.DebugLogPayload(ctx, "outbound_response_chunk", chunk)
				_ = enc.Encode(chunk)
				flusher.Flush()
			}
		}

		total, load, promptDur, evalDur := metrics.finalize()
		done := GenerateResponse{
			Model:              req.Model,
			CreatedAt:          time.Now(),
			Response:           "",
			Done:               true,
			DoneReason:         doneReason(metrics.finishReason),
			TotalDuration:      total,
			LoadDuration:       load,
			PromptEvalCount:    metrics.promptTokens,
			PromptEvalDuration: promptDur,
			EvalCount:          metrics.completionTokens,
			EvalDuration:       evalDur,
		}
		provider.DebugLogPayload(ctx, "outbound_response_chunk", done)
		if err := enc.Encode(done); err != nil {
			rl.Errorf("encode done: %v", err)
		}
		flusher.Flush()
		logCompletionForModel(ctx, rl, "generate-stream", req.Model, metrics)
		return
	}

	// Non-streaming branch.
	w.Header().Set("Content-Type", "application/json")
	resp, metrics, err := h.runNonStreamWithRetry(ctx, rl, req.Model, req.System, contents, genOpts)
	if err != nil {
		rl.Errorf("non-stream failed class=%s err=%v", classifyError(err), err)
		http.Error(w, fmt.Sprintf("Error generating content: %v", err), http.StatusBadGateway)
		return
	}

	var fullContent strings.Builder
	if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
		for _, part := range resp.Candidates[0].Content.Parts {
			fullContent.WriteString(part.Text)
		}
	}
	contentStr := fullContent.String()
	pub, _ := provider.ParsePublisher(req.Model)
	if pub == "google" {
		unescaper := NewEntityUnescaper()
		contentStr = unescaper.Process(contentStr) + unescaper.Flush()
	}
	total, load, promptDur, evalDur := metrics.finalize()
	genResp := GenerateResponse{
		Model:              req.Model,
		CreatedAt:          time.Now(),
		Response:           contentStr,
		Done:               true,
		DoneReason:         doneReason(metrics.finishReason),
		TotalDuration:      total,
		LoadDuration:       load,
		PromptEvalCount:    metrics.promptTokens,
		PromptEvalDuration: promptDur,
		EvalCount:          metrics.completionTokens,
		EvalDuration:       evalDur,
	}
	provider.DebugLogPayload(ctx, "outbound_response", genResp)
	if err := json.NewEncoder(w).Encode(genResp); err != nil {
		rl.Errorf("encode response: %v", err)
	}
	logCompletionForModel(ctx, rl, "generate", req.Model, metrics)
}
