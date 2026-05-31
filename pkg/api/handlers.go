package api

import (
	"go.f0o.dev/cline-vertex-gw/pkg/pipeline"
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

// HealthHandler now lives in health.go (split for clarity now that we have
// /healthz, /readyz, and /version alongside the legacy "/").

// familyFromName picks a reasonable Ollama-style "family" tag from a model id.
func familyFromName(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "gemini"):
		return "gemini"
	case strings.Contains(lower, "gemma"):
		return "gemma"
	case strings.Contains(lower, "claude"):
		return "claude"
	case strings.Contains(lower, "llama"):
		return "llama"
	case strings.Contains(lower, "mistral"), strings.Contains(lower, "mixtral"), strings.Contains(lower, "codestral"):
		return "mistral"
	case strings.Contains(lower, "jamba"):
		return "jamba"
	case strings.Contains(lower, "command"):
		return "command"
	case strings.Contains(lower, "deepseek"):
		return "deepseek"
	case strings.Contains(lower, "qwen"):
		return "qwen"
	}
	return "vertex"
}

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

// callMetrics captures timing & token usage for a single upstream call.
//
// cachedPromptTokens is the subset of promptTokens that the upstream served
// from a prompt cache (when supported, e.g. Anthropic cache_control). It's
// surfaced separately so operators can verify GW_ANTHROPIC_PROMPT_CACHE is
// actually saving money. cachedPromptTokens <= promptTokens by construction.
type callMetrics struct {
	requestStart       time.Time
	firstChunk         time.Time
	end                time.Time
	promptTokens       int32
	cachedPromptTokens int32
	completionTokens   int32
	finishReason       string
}

// final returns the durations Ollama expects (in nanoseconds):
//   - total_duration   : wall clock end-to-end
//   - load_duration    : approx connection/setup latency (time-to-first-byte)
//   - prompt_eval_dur  : same as load_duration (Vertex doesn't break it out)
//   - eval_duration    : time spent streaming the completion
func (m *callMetrics) finalize() (total, load, promptDur, evalDur int64) {
	if m.end.IsZero() {
		m.end = time.Now()
	}
	total = m.end.Sub(m.requestStart).Nanoseconds()
	if !m.firstChunk.IsZero() {
		load = m.firstChunk.Sub(m.requestStart).Nanoseconds()
		promptDur = load
		evalDur = m.end.Sub(m.firstChunk).Nanoseconds()
	} else {
		// No streaming happened (or non-stream call). Spread the time evenly.
		load = total / 2
		promptDur = load
		evalDur = total - load
	}
	if evalDur < 0 {
		evalDur = 0
	}
	if promptDur < 0 {
		promptDur = 0
	}
	return
}

// runStreamWithRetry executes an upstream streaming call with retry semantics.
// It retries only when no chunk has been emitted yet, since once chunks are
// flushed to the client we cannot safely retry without producing duplicates.
//
// onChunk is called for every text fragment received from upstream.
// The returned metrics include first-chunk and total durations for reporting.
//
// A per-attempt LoopDetector watches the cumulative output; if it spots a
// runaway repetition pattern it cancels the upstream context, terminating
// the stream early to bound cost. The finishReason is set to "length" so
// callers see a normal terminator (the partial output is already delivered).
func (h *APIHandler) runStreamWithRetry(
	ctx context.Context,
	rl *reqLogger,
	model string,
	system string,
	contents []*genai.Content,
	opts *pipeline.GenerationOptions,
	onChunk streamCallback,
) (callMetrics, error) {
	cfg := defaultRetry()
	metrics := callMetrics{requestStart: time.Now()}

	var lastErr error
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			class := classifyError(lastErr)
			MetricsRetry(class)
			rl.Warnf("stream retry attempt=%d after error class=%s err=%v",
				attempt, class, lastErr)
			if werr := cfg.waitFor(ctx, attempt-1); werr != nil {
				return metrics, werr
			}
		}

		// Per-attempt: a child context the detector can cancel, plus a fresh
		// detector instance (state doesn't carry across retries — we want a
		// clean window on each upstream call).
		attemptCtx, cancel := context.WithCancel(ctx)
		detector := NewLoopDetector()
		loopFired := false

		gotChunk := false
		var iterErr error

		for resp, err := range h.Vertex.GenerateStream(attemptCtx, model, system, contents, opts) {
			if err != nil {
				iterErr = err
				break
			}
			if !gotChunk {
				metrics.firstChunk = time.Now()
				rl.Logf("first chunk after %v (attempt=%d)", metrics.firstChunk.Sub(metrics.requestStart), attempt)
			}
			gotChunk = true

			if resp.UsageMetadata != nil {
				if resp.UsageMetadata.PromptTokenCount > 0 {
					metrics.promptTokens = resp.UsageMetadata.PromptTokenCount
				}
				if resp.UsageMetadata.CandidatesTokenCount > 0 {
					metrics.completionTokens = resp.UsageMetadata.CandidatesTokenCount
				}
				if resp.UsageMetadata.CachedContentTokenCount > 0 {
					metrics.cachedPromptTokens = resp.UsageMetadata.CachedContentTokenCount
				}
			}

			if len(resp.Candidates) > 0 {
				cand := resp.Candidates[0]
				if cand.FinishReason != "" {
					metrics.finishReason = string(cand.FinishReason)
				}
				if cand.Content != nil {
					for _, part := range cand.Content.Parts {
						// FunctionCall parts arrive as fully-assembled units
						// from every publisher adapter (OpenAI accumulates
						// streamed args internally; Anthropic emits on
						// content_block_stop; Cohere emits on
						// tool-calls-generation). Forward as-is.
						if part.FunctionCall != nil {
							if cberr := onChunk(StreamDelta{Part: part}); cberr != nil {
								cancel()
								metrics.end = time.Now()
								return metrics, cberr
							}
							continue
						}
						if part.Text == "" {
							continue
						}
						if cberr := onChunk(StreamDelta{Text: part.Text}); cberr != nil {
							cancel()
							metrics.end = time.Now()
							return metrics, cberr
						}
						// Feed the chunk to the loop detector; if it fires we
						// flag it, cancel the upstream context, and let the
						// iterator drain naturally on the next round.
						detector.Observe(part.Text)
						if !loopFired && detector.LoopDetected() {
							loopFired = true
							MetricsLoopDetectorFired()
							rl.Warnf("loop detector fired after %d output tokens; cancelling stream",
								metrics.completionTokens)
							cancel()
						}
					}
				}
			}
		}
		cancel() // always release the per-attempt ctx resources

		if iterErr == nil || (loopFired && errors.Is(iterErr, context.Canceled)) {
			// Either: normal completion, OR loop-detector-induced cancel
			// (which surfaces as ctx.Err()). Treat both as success from the
			// client's perspective; the partial output is already delivered.
			metrics.end = time.Now()
			if loopFired {
				metrics.finishReason = "MAX_TOKENS" // map → "length" downstream
			}
			return metrics, nil
		}

		lastErr = iterErr
		if gotChunk {
			// Chunks were emitted; we cannot safely retry.
			rl.Warnf("stream aborted after partial output (class=%s): %v",
				classifyError(iterErr), iterErr)
			metrics.end = time.Now()
			return metrics, iterErr
		}
		if !isRetryableError(iterErr) {
			rl.Errorf("stream non-retryable error (class=%s): %v",
				classifyError(iterErr), iterErr)
			metrics.end = time.Now()
			return metrics, iterErr
		}
		// loop and retry
	}
	metrics.end = time.Now()
	return metrics, fmt.Errorf("max retries exceeded: %w", lastErr)
}

// runNonStreamWithRetry executes a non-streaming upstream call with retry.
func (h *APIHandler) runNonStreamWithRetry(
	ctx context.Context,
	rl *reqLogger,
	model string,
	system string,
	contents []*genai.Content,
	opts *pipeline.GenerationOptions,
) (*genai.GenerateContentResponse, callMetrics, error) {
	cfg := defaultRetry()
	metrics := callMetrics{requestStart: time.Now()}

	var lastErr error
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			class := classifyError(lastErr)
			MetricsRetry(class)
			rl.Warnf("non-stream retry attempt=%d after error class=%s err=%v",
				attempt, class, lastErr)
			if werr := cfg.waitFor(ctx, attempt-1); werr != nil {
				return nil, metrics, werr
			}
		}

		resp, err := h.Vertex.Generate(ctx, model, system, contents, opts)
		if err == nil {
			metrics.firstChunk = time.Now()
			metrics.end = metrics.firstChunk
			if resp.UsageMetadata != nil {
				metrics.promptTokens = resp.UsageMetadata.PromptTokenCount
				metrics.completionTokens = resp.UsageMetadata.CandidatesTokenCount
				metrics.cachedPromptTokens = resp.UsageMetadata.CachedContentTokenCount
			}
			if len(resp.Candidates) > 0 && resp.Candidates[0].FinishReason != "" {
				metrics.finishReason = string(resp.Candidates[0].FinishReason)
			}
			return resp, metrics, nil
		}
		lastErr = err
		if !isRetryableError(err) {
			rl.Errorf("non-stream non-retryable error (class=%s): %v", classifyError(err), err)
			metrics.end = time.Now()
			return nil, metrics, err
		}
	}
	metrics.end = time.Now()
	return nil, metrics, fmt.Errorf("max retries exceeded: %w", lastErr)
}

// buildContents converts an Ollama-style messages array into Vertex-compatible
// alternating Content blocks plus a concatenated system prompt.
//
// Tool calling: assistant turns carrying `tool_calls` are preserved as
// FunctionCall parts on the assistant turn (instead of being skipped for
// having empty text). `role:"tool"` messages become FunctionResponse parts
// on a user-role turn — same translation strategy as the OpenAI surface so
// the publisher adapters see a consistent shape regardless of source.
//
// Multimodal: a message's `Images` field (Ollama-native bare-base64 array)
// is decoded into InlineData parts. MIME type is sniffed from the magic
// bytes (Ollama itself sniffs the same way). Per-part and per-request size
// caps apply (GW_MAX_IMAGE_BYTES_PER_{PART,REQUEST}).
//
// Returns a clear error (which the handler maps to HTTP 400) on bad image
// data or oversized requests.
func buildContents(messages []Message) (contents []*genai.Content, systemPrompt string, err error) {
	var lastRole string
	var currentParts []*genai.Part
	totalMediaBytesSeen := 0

	flush := func() {
		if lastRole != "" && len(currentParts) > 0 {
			contents = append(contents, &genai.Content{Role: lastRole, Parts: currentParts})
		}
	}

	for msgIdx, msg := range messages {
		role := strings.ToLower(msg.Role)
		if role == "system" {
			if msg.Content != "" {
				systemPrompt += msg.Content + "\n"
			}
			// Images on a system turn are silently dropped — no upstream
			// supports them as system context.
			continue
		}

		// Decode any inline images attached to this message. The order is
		// "text first, then images" because the Ollama wire shape carries
		// them in two distinct fields; there is no client-supplied
		// ordering to preserve (unlike the OpenAI parts array).
		var msgParts []*genai.Part
		if msg.Content != "" {
			msgParts = append(msgParts, &genai.Part{Text: msg.Content})
		}
		for i, b64 := range msg.Images {
			mime, data, derr := decodeOllamaImage(b64)
			if derr != nil {
				return nil, "", fmt.Errorf("message %d image %d: %w", msgIdx, i, derr)
			}
			totalMediaBytesSeen += len(data)
			if totalMediaBytesSeen > maxMediaBytesPerRequest {
				return nil, "", fmt.Errorf("request media payload exceeds %d bytes (GW_MAX_MEDIA_BYTES_PER_REQUEST)",
					maxMediaBytesPerRequest)
			}
			msgParts = append(msgParts, &genai.Part{
				InlineData: &genai.Blob{MIMEType: mime, Data: data},
			})
		}

		switch role {
		case "assistant":
			for _, tc := range msg.ToolCalls {
				if tc.Function.Name == "" {
					continue
				}
				msgParts = append(msgParts, ollamaToolCallToGenaiPart(tc))
			}
		case "tool":
			// Replace text with a FunctionResponse part so we don't ship
			// the result twice (once as text and once as the structured
			// response). Tool results are conventionally a "user" turn
			// from the model's perspective. Images on a tool message are
			// dropped — tool results are by definition text-shaped.
			msgParts = []*genai.Part{toolResultPart(msg.ToolCallID, msg.ToolName, msg.Content)}
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

// genOptionsFromAPI converts Ollama Options + tools into the provider's
// internal form. The tools argument is the request's top-level Tools slice
// (Ollama puts tool definitions outside Options, mirroring real Ollama's
// shape); when non-empty it's translated to genai.Tool definitions on the
// returned options.
func genOptionsFromAPI(o *Options, tools []ToolDef) *pipeline.GenerationOptions {
	hasAny := false
	out := &pipeline.GenerationOptions{}
	if o != nil {
		if o.Temperature != 0 {
			out.Temperature = &o.Temperature
			hasAny = true
		}
		if o.TopP != 0 {
			out.TopP = &o.TopP
			hasAny = true
		}
		if o.TopK != 0 {
			out.TopK = &o.TopK
			hasAny = true
		}
		if len(o.Stop) > 0 {
			out.Stop = o.Stop
			hasAny = true
		}
		if o.NumPredict != 0 {
			out.MaxTokens = &o.NumPredict
			hasAny = true
		}
	}
	if t := translateOllamaToolsToGenai(tools); len(t) > 0 {
		out.Tools = t
		hasAny = true
	}
	if !hasAny {
		return nil
	}
	return out
}

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

// doneReason normalizes Vertex finish reasons into Ollama-style strings.
// Cline reads this to detect "stop" vs other terminal conditions.
func doneReason(vertexReason string) string {
	switch strings.ToUpper(vertexReason) {
	case "", "STOP", "FINISH_REASON_STOP":
		return "stop"
	case "MAX_TOKENS", "FINISH_REASON_MAX_TOKENS":
		return "length"
	case "SAFETY", "FINISH_REASON_SAFETY":
		return "safety"
	case "RECITATION", "FINISH_REASON_RECITATION":
		return "recitation"
	default:
		return strings.ToLower(vertexReason)
	}
}

// logCompletion emits a single concise summary line at request end AND
// feeds the Prometheus token / duration counters.
//
// cached_tok is the number of prompt tokens served from a prompt cache; when
// caching is configured (e.g. GW_ANTHROPIC_PROMPT_CACHE=on) it lets operators
// quickly see whether requests are hitting the cache. cached_pct is the share
// of prompt tokens that were cache-served — 0% on a cold session, ~95%+ on
// well-warmed multi-turn Cline sessions.
//
// logCompletionForModel wraps this with model labels so the per-model token
// counters get populated; callers that only know the route (not the model)
// can use logCompletion which feeds duration-only metrics.
func logCompletionForModel(ctx context.Context, rl *reqLogger, label, model string, m callMetrics) {
	logCompletion(rl, label, m)
	if model != "" {
		MetricsTokens("prompt", model, m.promptTokens-m.cachedPromptTokens)
		MetricsTokens("cached", model, m.cachedPromptTokens)
		MetricsTokens("completion", model, m.completionTokens)
		logAndMeterCost(ctx, rl, label, model, m)
	}
	total, _, _, _ := m.finalize()
	MetricsRequest(label, doneReason(m.finishReason), float64(total)/1e9)
}

// logAndMeterCost computes a per-request USD estimate from the live pricing
// table (scraped from the Cloud Billing Catalog API), prints a breakdown to
// the console alongside the request stats, and feeds the cumulative cost
// metric. When pricing for the model is unknown it prints "cost=unavailable"
// and skips the metric (no $0 noise). Cached prompt tokens are billed at the
// reduced cached-input rate; the remaining prompt tokens at the input rate.
func logAndMeterCost(ctx context.Context, rl *reqLogger, label, model string, m callMetrics) {
	bd, ok := provider.EstimateCost(ctx, model, m.promptTokens, m.cachedPromptTokens, m.completionTokens)
	if !ok {
		rl.L().Info("cost unavailable", "phase", label, "model", model, "reason", "no-pricing")
		return
	}
	rl.L().Info("cost estimate",
		"phase", label, "model", model, "approx", bd.Approx,
		"total_usd", bd.TotalUSD, "input_usd", bd.InputUSD,
		"cached_usd", bd.CachedUSD, "output_usd", bd.OutputUSD,
		"input_per_mtok", bd.InputPerM, "cached_per_mtok", bd.CachedPerM,
		"output_per_mtok", bd.OutputPerM, "src", bd.Source,
	)
	tier := "standard"
	if ctx != nil {
		if t, ok := ctx.Value(provider.ContextKeyRoutingTier).(string); ok && t != "" {
			tier = t
		}
	}
	MetricsEstimatedCost("input", model, tier, bd.InputUSD)
	MetricsEstimatedCost("cached", model, tier, bd.CachedUSD)
	MetricsEstimatedCost("output", model, tier, bd.OutputUSD)
}

func logCompletion(rl *reqLogger, label string, m callMetrics) {
	total, load, _, evalDur := m.finalize()
	var tps float64
	if evalDur > 0 && m.completionTokens > 0 {
		tps = float64(m.completionTokens) / (float64(evalDur) / 1e9)
	}
	var cachedPct float64
	if m.promptTokens > 0 {
		cachedPct = 100.0 * float64(m.cachedPromptTokens) / float64(m.promptTokens)
	}
	rl.L().Info("request done",
		"phase", label,
		"total", time.Duration(total).Truncate(time.Millisecond).String(),
		"load", time.Duration(load).Truncate(time.Millisecond).String(),
		"eval", time.Duration(evalDur).Truncate(time.Millisecond).String(),
		"prompt_tok", m.promptTokens,
		"cached_tok", m.cachedPromptTokens,
		"cached_pct", cachedPct,
		"eval_tok", m.completionTokens,
		"tps", tps,
		"reason", doneReason(m.finishReason),
	)
}
