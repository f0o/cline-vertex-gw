package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.f0o.dev/cline-vertex-gw/pkg/cache"
	"go.f0o.dev/cline-vertex-gw/pkg/pipeline"
	"go.f0o.dev/cline-vertex-gw/pkg/provider"
	"google.golang.org/genai"
)

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
	retrievalsCount := 0

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
		var retrievalCall *genai.FunctionCall

		for resp, err := range h.Vertex.GenerateStream(attemptCtx, model, system, contents, opts) {
			if err != nil {
				iterErr = err
				break
			}
			if !gotChunk && retrievalCall == nil {
				metrics.firstChunk = time.Now()
				rl.Logf("first chunk after %v (attempt=%d)", metrics.firstChunk.Sub(metrics.requestStart), attempt)
			}

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
						if part.FunctionCall != nil && part.FunctionCall.Name == "retrieve_elided_content" {
							retrievalCall = part.FunctionCall
							continue // Withhold retrieval chunks from the client completely
						}
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
							gotChunk = true
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
						gotChunk = true
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

		if iterErr == nil {
			if retrievalCall != nil {
				hash, _ := retrievalCall.Args["hash"].(string)
				if hash != "" {
					if retrievalsCount < 5 {
						retrievalsCount++
						var rawContent string
						fileName := "elided_" + hash + ".json"
						fresh, err := cache.ReadFSCache(fileName, &rawContent)
						if err != nil || !fresh || rawContent == "" {
							rawContent = "[Error: cached content expired or unavailable]"
						}
						callID := retrievalCall.ID
						if callID == "" {
							callID = "call_" + newReqID()
						}
						modelTurn := &genai.Content{
							Role: genai.RoleModel,
							Parts: []*genai.Part{{
								FunctionCall: &genai.FunctionCall{
									ID:   callID,
									Name: "retrieve_elided_content",
									Args: map[string]any{"hash": hash},
								},
							}},
						}
						userTurn := &genai.Content{
							Role: genai.RoleUser,
							Parts: []*genai.Part{{
								FunctionResponse: &genai.FunctionResponse{
									ID:       callID,
									Name:     "retrieve_elided_content",
									Response: map[string]any{"output": rawContent},
								},
							}},
						}
						contents = append(contents, modelTurn, userTurn)
						rl.Logf("intercepted retrieve_elided_content in stream; resolving from cache and restarting stream (count=%d)", retrievalsCount)
						attempt = -1 // Restart the attempts loop
						continue
					}
				}
			}
		}

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
	retrievalsCount := 0

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
			// Check if we need to resolve a retrieval tool call
			if resolved, newContents := resolveRetrievalToolCall(contents, resp.Candidates); resolved {
				if retrievalsCount < 5 {
					retrievalsCount++
					rl.Logf("intercepted retrieve_elided_content; resolving from cache and retrying generation (count=%d)", retrievalsCount)
					contents = newContents
					attempt = -1 // Reset the attempts loop back to the start!
					continue
				}
			}

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

// resolveRetrievalToolCall checks if any candidate contains a FunctionCall to "retrieve_elided_content".
// If so, it reads the original content from the cache, appends a model turn carrying the call
// and a user turn carrying the FunctionResponse to contents, and returns (true, updatedContents).
// Otherwise it returns (false, contents).
func resolveRetrievalToolCall(contents []*genai.Content, candidates []*genai.Candidate) (bool, []*genai.Content) {
	if len(candidates) == 0 || candidates[0].Content == nil {
		return false, contents
	}
	var targetCall *genai.FunctionCall
	for _, p := range candidates[0].Content.Parts {
		if p != nil && p.FunctionCall != nil && p.FunctionCall.Name == "retrieve_elided_content" {
			targetCall = p.FunctionCall
			break
		}
	}
	if targetCall == nil {
		return false, contents
	}

	hash, _ := targetCall.Args["hash"].(string)
	if hash == "" {
		return false, contents
	}

	// Read content from filesystem cache
	var rawContent string
	fileName := "elided_" + hash + ".json"
	fresh, err := cache.ReadFSCache(fileName, &rawContent)
	if err != nil || !fresh || rawContent == "" {
		rawContent = "[Error: cached content expired or unavailable]"
	}

	callID := targetCall.ID
	if callID == "" {
		callID = "call_" + newReqID()
	}

	// Construct model turn and user turn
	modelTurn := &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				ID:   callID,
				Name: "retrieve_elided_content",
				Args: map[string]any{"hash": hash},
			},
		}},
	}
	userTurn := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       callID,
				Name:     "retrieve_elided_content",
				Response: map[string]any{"output": rawContent},
			},
		}},
	}

	// Append to a copy of contents
	newContents := make([]*genai.Content, len(contents)+2)
	copy(newContents, contents)
	newContents[len(contents)] = modelTurn
	newContents[len(contents)+1] = userTurn

	return true, newContents
}
