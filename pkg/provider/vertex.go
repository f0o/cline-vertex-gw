package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/pipeline"
	"iter"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/genai"
)

type ContextKey string

const (
	ContextKeyReqID       ContextKey = "req_id"
	ContextKeyRoute       ContextKey = "route"
	ContextKeyRoutingTier ContextKey = "routing_tier"
)

// cloudPlatformScope is the OAuth2 scope required for all Vertex AI / Cloud
// Billing REST calls made by this package.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// dumpPayloads dictates whether DebugLogPayload actually emits payloads to logs.
// Defaults to false to avoid bloating logs even when LOG_LEVEL=debug.
var dumpPayloads = envBool("GW_DUMP_PAYLOADS", false)

// DebugLogPayload serializes the payload as JSON and logs it using slog.DebugContext.
func DebugLogPayload(ctx context.Context, step string, payload any) {
	if !dumpPayloads {
		return
	}

	reqID, _ := ctx.Value(ContextKeyReqID).(string)
	route, _ := ctx.Value(ContextKeyRoute).(string)

	var payloadStr string
	if b, err := json.Marshal(payload); err == nil {
		payloadStr = string(b)
	} else {
		payloadStr = fmt.Sprintf("%+v", payload)
	}

	slog.DebugContext(ctx, "payload log",
		slog.String("step", step),
		slog.String("req_id", reqID),
		slog.String("route", route),
		slog.String("payload", payloadStr),
	)
}

// VertexClient wraps a genai client and exposes higher-level helpers used by
// the Ollama-compatible HTTP API.
type VertexClient struct {
	client     *genai.Client
	projectID  string
	location   string
	httpClient *http.Client // authenticated client for short REST calls (15s timeout)
	streamHTTP *http.Client // authenticated client for streaming calls (no timeout)
}

func NewVertexClient(ctx context.Context, projectID, location string) (*VertexClient, error) {
	sdkHTTPClient, err := google.DefaultClient(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("error creating authenticated sdk http client: %w", err)
	}
	sdkHTTPClient.Transport = &routingTierRoundTripper{underlying: sdkHTTPClient.Transport}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend:    genai.BackendVertexAI,
		Project:    projectID,
		Location:   location,
		HTTPClient: sdkHTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating Vertex AI client: %w", err)
	}

	httpClient, err := google.DefaultClient(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("error creating authenticated http client: %w", err)
	}
	httpClient.Transport = &routingTierRoundTripper{underlying: httpClient.Transport}

	// Apply a sane timeout so a hung discovery call can't stall /api/tags.
	httpClient.Timeout = 15 * time.Second

	// Separate client for streaming/generation calls — these legitimately run
	// for many seconds, so we rely on the request context for cancellation
	// rather than a fixed Timeout that would truncate long completions.
	streamHTTP, err := google.DefaultClient(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("error creating authenticated streaming http client: %w", err)
	}
	streamHTTP.Transport = &routingTierRoundTripper{underlying: streamHTTP.Transport}

	return &VertexClient{
		client:     client,
		projectID:  projectID,
		location:   location,
		httpClient: httpClient,
		streamHTTP: streamHTTP,
	}, nil
}

func (vc *VertexClient) Close() {
	// genai.Client does not expose a Close in the public interface; the
	// underlying transport will be garbage-collected with the process.
}

// publisherEndpoint builds a `:rawPredict` / `:streamRawPredict` URL for any
// publisher on Vertex AI. The `global` location uses the bare
// `aiplatform.googleapis.com` host; every other location uses
// `<location>-aiplatform.googleapis.com`. There is no
// `global-aiplatform.googleapis.com`.
func (vc *VertexClient) publisherEndpoint(publisher, modelID, method string) string {
	host := "https://aiplatform.googleapis.com"
	if vc.location != "" && vc.location != "global" {
		host = fmt.Sprintf("https://%s-aiplatform.googleapis.com", vc.location)
	}
	return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/%s/models/%s:%s",
		host, vc.projectID, vc.location, publisher, modelID, method)
}

// useGoogleSearchField determines if the given model ID should use the new
// GoogleSearch field instead of GoogleSearchRetrieval.
func useGoogleSearchField(modelName string) bool {
	lower := strings.ToLower(modelName)
	if lower == "" {
		return true // default to new field if empty/unknown
	}
	// Older 1.5 and 1.0 models on Vertex AI only support GoogleSearchRetrieval
	if strings.Contains(lower, "gemini-1.5") || strings.Contains(lower, "gemini-1.0") {
		return false
	}
	return true
}

func (vc *VertexClient) GetConfig(modelName string, systemPrompt string, opts *pipeline.GenerationOptions) *genai.GenerateContentConfig {
	var config genai.GenerateContentConfig
	if systemPrompt != "" {
		config.SystemInstruction = &genai.Content{
			Parts: []*genai.Part{{Text: systemPrompt}},
		}
	}
	useNewSearch := useGoogleSearchField(modelName)
	if opts != nil {
		config.Temperature = opts.Temperature
		config.TopP = opts.TopP
		if opts.TopK != nil {
			topKFloat := float32(*opts.TopK)
			config.TopK = &topKFloat
		}
		config.StopSequences = opts.Stop
		if opts.MaxTokens != nil {
			config.MaxOutputTokens = *opts.MaxTokens
		}
		// Tool calling: pass-through to the genai SDK, which speaks Gemini's
		// native shape natively. Other publishers receive Tools/ToolConfig
		// directly from opts inside their per-adapter Build* helpers.
		if len(opts.Tools) > 0 {
			config.Tools = make([]*genai.Tool, len(opts.Tools))
			for i, tool := range opts.Tools {
				if tool != nil && tool.GoogleSearchRetrieval != nil && useNewSearch {
					// Map old search retrieval tool to the new GoogleSearch tool for newer models
					config.Tools[i] = &genai.Tool{
						GoogleSearch: &genai.GoogleSearch{},
					}
				} else {
					config.Tools[i] = tool
				}
			}
		}
		if opts.ToolConfig != nil {
			config.ToolConfig = opts.ToolConfig
		}
	}

	// Inject configured search grounding tool if present
	if searchTool := vc.getSearchGroundingTool(modelName); searchTool != nil {
		config.Tools = append(config.Tools, searchTool)
	}

	return &config
}

// getSearchGroundingTool returns a genai.Tool populated with the configured search grounding type
// (e.g. GoogleSearchRetrieval or EnterpriseWebSearch or GoogleSearch) governed by GW_GEMINI_SEARCH_GROUNDING.
func (vc *VertexClient) getSearchGroundingTool(modelName string) *genai.Tool {
	grounding := envString("GW_GEMINI_SEARCH_GROUNDING", "")
	if grounding == "" {
		return nil
	}

	useNewSearch := useGoogleSearchField(modelName)

	switch strings.ToLower(grounding) {
	case "google_search", "google-search", "google_search_retrieval", "google-search-retrieval":
		if useNewSearch {
			return &genai.Tool{
				GoogleSearch: &genai.GoogleSearch{},
			}
		}
		t := &genai.Tool{
			GoogleSearchRetrieval: &genai.GoogleSearchRetrieval{},
		}
		thresholdVal := envFloat32("GW_GEMINI_SEARCH_THRESHOLD", -1.0)
		if thresholdVal >= 0 {
			t.GoogleSearchRetrieval.DynamicRetrievalConfig = &genai.DynamicRetrievalConfig{
				DynamicThreshold: &thresholdVal,
				Mode:             "MODE_DYNAMIC",
			}
		}
		return t
	case "enterprise_web_search", "enterprise-web-search":
		return &genai.Tool{
			EnterpriseWebSearch: &genai.EnterpriseWebSearch{},
		}
	}
	return nil
}

func (vc *VertexClient) GenerateStream(ctx context.Context, modelName string, systemPrompt string, contents []*genai.Content, opts *pipeline.GenerationOptions) iter.Seq2[*genai.GenerateContentResponse, error] {
	publisher, modelID := ParsePublisher(modelName)
	kind, ok := PublisherKind(publisher)
	if !ok {
		err := errUnsupportedPublisher(publisher, modelID)
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(nil, err)
		}
	}

	// Apply gateway-wide output-token caps (GW_DEFAULT_MAX_OUTPUT_TOKENS /
	// GW_MAX_OUTPUT_TOKENS_HARD) before dispatching to any publisher adapter.
	// Doing it here means every adapter inherits the clamp without per-adapter
	// edits, and a future new publisher gets it for free.
	opts = ApplyOutputCaps(opts)
	// Compression pipeline. Order is load-bearing — see comment in
	// applyCompressionPipeline for the rationale.
	contents, systemPrompt = pipeline.ApplyCompressionPipeline(contents, systemPrompt, kind == adapterGoogle, opts)

	switch kind {
	case adapterAnthropic:
		return vc.anthropicGenerateStream(ctx, modelID, systemPrompt, contents, opts)
	case adapterCohere:
		return vc.cohereGenerateStream(ctx, modelID, systemPrompt, contents, opts)
	case adapterOpenAICompat:
		return vc.openaiGenerateStream(ctx, publisher, modelID, systemPrompt, contents, opts)
	case adapterGoogle:
		config := vc.GetConfig(modelName, systemPrompt, opts)
		// Explicit Gemini prompt caching (CachedContent). The shared planner
		// decides whether the system+tools prefix is worth caching; this is a
		// best-effort no-op when it isn't (or on any error).
		plan := PlanCache(contents, strings.TrimSpace(systemPrompt), "google")
		vc.MaybeApplyGeminiCache(ctx, modelName, strings.TrimSpace(systemPrompt), contents, config, plan)
		fullName := FormatModelName(modelName)
		DebugLogPayload(ctx, "upstream_request", map[string]any{
			"model":    fullName,
			"contents": contents,
			"config":   config,
		})
		stream := vc.client.Models.GenerateContentStream(ctx, fullName, contents, config)

		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			for chunk, err := range stream {
				if err == nil {
					DebugLogPayload(ctx, "upstream_response_chunk", chunk)
				}
				if !yield(chunk, err) {
					return
				}
			}
		}
	default:
		// Fail fast with a clear message rather than letting the SDK return
		// the misleading "is not servable in region global" error.
		err := errUnsupportedPublisher(publisher, modelID)
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(nil, err)
		}
	}
}

func (vc *VertexClient) Generate(ctx context.Context, modelName string, systemPrompt string, contents []*genai.Content, opts *pipeline.GenerationOptions) (*genai.GenerateContentResponse, error) {
	publisher, modelID := ParsePublisher(modelName)
	kind, ok := PublisherKind(publisher)
	if !ok {
		return nil, errUnsupportedPublisher(publisher, modelID)
	}

	opts = ApplyOutputCaps(opts) // see GenerateStream for rationale.
	contents, systemPrompt = pipeline.ApplyCompressionPipeline(contents, systemPrompt, kind == adapterGoogle, opts)

	switch kind {
	case adapterAnthropic:
		return vc.anthropicGenerate(ctx, modelID, systemPrompt, contents, opts)
	case adapterCohere:
		return vc.cohereGenerate(ctx, modelID, systemPrompt, contents, opts)
	case adapterOpenAICompat:
		return vc.openaiGenerate(ctx, publisher, modelID, systemPrompt, contents, opts)
	case adapterGoogle:
		config := vc.GetConfig(modelName, systemPrompt, opts)
		fullName := FormatModelName(modelName)
		DebugLogPayload(ctx, "upstream_request", map[string]any{
			"model":    fullName,
			"contents": contents,
			"config":   config,
		})
		resp, err := vc.client.Models.GenerateContent(ctx, fullName, contents, config)
		if err == nil {
			DebugLogPayload(ctx, "upstream_response", resp)
		}
		return resp, err
	default:
		return nil, errUnsupportedPublisher(publisher, modelID)
	}
}
