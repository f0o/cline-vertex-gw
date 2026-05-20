package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/genai"
)

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
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  projectID,
		Location: location,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating Vertex AI client: %w", err)
	}

	httpClient, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("error creating authenticated http client: %w", err)
	}
	// Apply a sane timeout so a hung discovery call can't stall /api/tags.
	httpClient.Timeout = 15 * time.Second

	// Separate client for streaming/generation calls — these legitimately run
	// for many seconds, so we rely on the request context for cancellation
	// rather than a fixed Timeout that would truncate long completions.
	streamHTTP, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("error creating authenticated streaming http client: %w", err)
	}

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


// GenerationOptions is the provider-side representation of generation tuning
// parameters. Pointer fields let callers omit settings without confusing them
// with explicit zero values.
//
// Tool-calling fields:
//   - Tools:      the function declarations the model is allowed to call this
//                 turn. Pass-through to per-publisher adapters which translate
//                 to each upstream's native shape. nil/empty disables tools.
//   - ToolConfig: the upstream-neutral tool_choice / function-calling mode
//                 (AUTO / ANY / NONE / specific function name). Optional; nil
//                 means the publisher's default ("auto" for every adapter).
//
// We deliberately reuse genai.Tool / genai.ToolConfig as the internal lingua
// franca — same pattern as everything else in this struct — so that adding a
// new publisher only requires translating a stable in-process type to the
// upstream's wire shape, and so the Google/Gemini path can pass them through
// to the genai SDK unchanged.
type GenerationOptions struct {
	Temperature *float32
	TopP        *float32
	TopK        *int32
	Stop        []string
	MaxTokens   *int32
	Tools       []*genai.Tool
	ToolConfig  *genai.ToolConfig
}

func (vc *VertexClient) GetConfig(systemPrompt string, opts *GenerationOptions) *genai.GenerateContentConfig {
	var config genai.GenerateContentConfig
	if systemPrompt != "" {
		config.SystemInstruction = &genai.Content{
			Parts: []*genai.Part{{Text: systemPrompt}},
		}
	}
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
			config.Tools = opts.Tools
		}
		if opts.ToolConfig != nil {
			config.ToolConfig = opts.ToolConfig
		}
	}
	return &config
}

// FormatModelName ensures the model name has the correct publisher prefix for
// Vertex AI. Google's Gemini models are accepted bare by the SDK; everything
// else must be addressed as "publishers/<publisher>/models/<id>".
func FormatModelName(model string) string {
	if strings.HasPrefix(model, "publishers/") {
		return model
	}
	publisher, _ := ParsePublisher(model)
	if publisher == "google" {
		return model
	}
	return fmt.Sprintf("publishers/%s/models/%s", publisher, model)
}

// ParsePublisher splits a model identifier into its publisher namespace and
// bare model id. It handles three input shapes:
//   - already-qualified resource paths: "publishers/anthropic/models/claude-..."
//   - short ids:                        "claude-opus-4-7"
//   - prefixed short ids:               "anthropic/claude-opus-4-7"
//
// When the publisher cannot be determined from the name (a bare "gemini-..."
// or unknown id), it defaults to "google" since that's what the genai SDK
// targets by default.
func ParsePublisher(model string) (publisher, modelID string) {
	if strings.HasPrefix(model, "publishers/") {
		// publishers/<pub>/models/<id>
		parts := strings.Split(model, "/")
		if len(parts) >= 4 {
			return parts[1], parts[3]
		}
	}
	// "anthropic/claude-opus-4-7" style
	if idx := strings.Index(model, "/"); idx > 0 && !strings.Contains(model[:idx], ".") {
		head := model[:idx]
		switch head {
		case "anthropic", "google", "meta", "mistralai", "ai21", "cohere",
			"deepseek-ai", "qwen", "nvidia", "moonshotai":
			return head, model[idx+1:]
		}
	}

	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "claude"):
		return "anthropic", model
	case strings.Contains(lower, "llama"):
		return "meta", model
	case strings.Contains(lower, "mistral"),
		strings.Contains(lower, "mixtral"),
		strings.Contains(lower, "codestral"):
		return "mistralai", model
	case strings.Contains(lower, "jamba"):
		return "ai21", model
	case strings.Contains(lower, "command"):
		return "cohere", model
	case strings.Contains(lower, "deepseek"):
		return "deepseek-ai", model
	case strings.Contains(lower, "qwen"):
		return "qwen", model
	}
	return "google", model
}

// openaiCompatPublishers enumerates the Vertex publishers that share the
// OpenAI-compatible chat-completions body shape against
// `:rawPredict` / `:streamRawPredict`.
var openaiCompatPublishers = map[string]bool{
	"meta":        true, // Llama 3.x / 4 (MaaS)
	"mistralai":   true, // Mistral Large, Codestral, Mixtral
	"ai21":        true, // Jamba 1.5 / 2.x
	"deepseek-ai": true, // DeepSeek
	"qwen":        true, // Qwen
	"nvidia":      true, // Nvidia-hosted MaaS models
}

// errUnsupportedPublisher is returned by Generate/GenerateStream when the
// inferred publisher namespace doesn't have a Vertex AI adapter implemented
// in this gateway yet. It surfaces a clearer error than the underlying SDK's
// misleading "is not servable in region ..." message.
func errUnsupportedPublisher(publisher, modelID string) error {
	return fmt.Errorf(
		"unsupported publisher %q (model %q): no Vertex AI adapter implemented in this gateway",
		publisher, modelID)
}

func (vc *VertexClient) GenerateStream(ctx context.Context, modelName string, systemPrompt string, contents []*genai.Content, opts *GenerationOptions) iter.Seq2[*genai.GenerateContentResponse, error] {
	publisher, modelID := ParsePublisher(modelName)
	// Apply gateway-wide output-token caps (GW_DEFAULT_MAX_OUTPUT_TOKENS /
	// GW_MAX_OUTPUT_TOKENS_HARD) before dispatching to any publisher adapter.
	// Doing it here means every adapter inherits the clamp without per-adapter
	// edits, and a future new publisher gets it for free.
	opts = ApplyOutputCaps(opts)
	// Compression pipeline. Order is load-bearing — see comment in
	// applyCompressionPipeline for the rationale.
	contents, systemPrompt = applyCompressionPipeline(contents, systemPrompt)
	switch {
	case publisher == "anthropic":
		return vc.anthropicGenerateStream(ctx, modelID, systemPrompt, contents, opts)
	case publisher == "cohere":
		return vc.cohereGenerateStream(ctx, modelID, systemPrompt, contents, opts)
	case openaiCompatPublishers[publisher]:
		return vc.openaiGenerateStream(ctx, publisher, modelID, systemPrompt, contents, opts)
	case publisher == "google":
		config := vc.GetConfig(systemPrompt, opts)
		fullName := FormatModelName(modelName)
		return vc.client.Models.GenerateContentStream(ctx, fullName, contents, config)
	default:
		// Fail fast with a clear message rather than letting the SDK return
		// the misleading "is not servable in region global" error.
		err := errUnsupportedPublisher(publisher, modelID)
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(nil, err)
		}
	}
}

func (vc *VertexClient) Generate(ctx context.Context, modelName string, systemPrompt string, contents []*genai.Content, opts *GenerationOptions) (*genai.GenerateContentResponse, error) {
	publisher, modelID := ParsePublisher(modelName)
	opts = ApplyOutputCaps(opts) // see GenerateStream for rationale.
	contents, systemPrompt = applyCompressionPipeline(contents, systemPrompt)
	switch {
	case publisher == "anthropic":
		return vc.anthropicGenerate(ctx, modelID, systemPrompt, contents, opts)
	case publisher == "cohere":
		return vc.cohereGenerate(ctx, modelID, systemPrompt, contents, opts)
	case openaiCompatPublishers[publisher]:
		return vc.openaiGenerate(ctx, publisher, modelID, systemPrompt, contents, opts)
	case publisher == "google":
		config := vc.GetConfig(systemPrompt, opts)
		fullName := FormatModelName(modelName)
		return vc.client.Models.GenerateContent(ctx, fullName, contents, config)
	default:
		return nil, errUnsupportedPublisher(publisher, modelID)
	}
}



// supportedPublishers enumerates the publisher namespaces we attempt to list.
// Order doesn't matter; results are merged and deduplicated.
var supportedPublishers = []string{
	"google",
	"anthropic",
	"meta",
	"mistralai",
	"ai21",
	"cohere",
	"deepseek-ai",
	"qwen",
	"nvidia",
}

// ListModels returns the union of models exposed by Vertex AI across all
// supported publishers, including 3rd-party (Claude, Llama, Mistral, etc.)
// models.
//
// Discovery strategy per publisher:
//  1. project+location scoped endpoint (preferred so we honor regional access)
//  2. us-central1 fallback endpoint
//  3. unauthenticated-by-project global catalog (`/v1beta1/publishers/X/models`)
//
// Additionally any deployed/tuned models returned by the SDK's Models.List()
// are merged in. All failures are logged but never abort the overall list so
// that a single bad publisher doesn't blank out /api/tags.
func (vc *VertexClient) ListModels(ctx context.Context) ([]*genai.Model, error) {
	start := time.Now()

	type publisherResult struct {
		publisher string
		models    []*genai.Model
		source    string // which endpoint succeeded
		err       error
	}

	results := make(chan publisherResult, len(supportedPublishers))
	var wg sync.WaitGroup
	for _, p := range supportedPublishers {
		wg.Add(1)
		go func(pub string) {
			defer wg.Done()
			models, source, err := vc.fetchPublisherModels(ctx, pub)
			results <- publisherResult{publisher: pub, models: models, source: source, err: err}
		}(p)
	}
	wg.Wait()
	close(results)

	seen := make(map[string]bool)
	var combined []*genai.Model
	for r := range results {
		if r.err != nil {
			log.Printf("[tags] publisher=%s: discovery failed: %v", r.publisher, r.err)
			continue
		}
		added := 0
		for _, m := range r.models {
			baseName := lastSegment(m.Name)
			if baseName == "" || seen[baseName] {
				continue
			}
			if !isChatModel(m) {
				continue
			}
			combined = append(combined, m)
			seen[baseName] = true
			added++
		}
		log.Printf("[tags] publisher=%s source=%s discovered=%d added=%d",
			r.publisher, r.source, len(r.models), added)
	}

	// Also pull in deployed/tuned models accessible to the project.
	deployed, derr := vc.listDeployedModels(ctx)
	if derr != nil {
		log.Printf("[tags] deployed models lookup failed: %v", derr)
	}
	deployedAdded := 0
	for _, m := range deployed {
		baseName := lastSegment(m.Name)
		if baseName == "" || seen[baseName] {
			continue
		}
		if !isChatModel(m) {
			continue
		}
		combined = append(combined, m)
		seen[baseName] = true
		deployedAdded++
	}
	if len(deployed) > 0 || derr == nil {
		log.Printf("[tags] deployed discovered=%d added=%d", len(deployed), deployedAdded)
	}

	// Stable ordering makes the /api/tags output deterministic across calls.
	sort.SliceStable(combined, func(i, j int) bool {
		return combined[i].Name < combined[j].Name
	})

	log.Printf("[tags] total models=%d publishers=%d elapsed=%v",
		len(combined), len(supportedPublishers), time.Since(start))
	return combined, nil
}

// fetchPublisherModels probes a chain of endpoints for the given publisher and
// returns the first non-empty result.
func (vc *VertexClient) fetchPublisherModels(ctx context.Context, publisher string) ([]*genai.Model, string, error) {
	endpoints := vc.publisherListEndpoints(publisher)

	var lastErr error
	for _, ep := range endpoints {
		models, err := vc.getModelsFromEndpoint(ctx, ep.url)
		if err != nil {
			lastErr = err
			log.Printf("[tags] publisher=%s endpoint=%s error: %v", publisher, ep.label, err)
			continue
		}
		if len(models) > 0 {
			return models, ep.label, nil
		}
		log.Printf("[tags] publisher=%s endpoint=%s returned 0 models", publisher, ep.label)
	}
	// If nothing failed but nothing returned either, that's still a success
	// (just no models exposed for this publisher in this account/region).
	if lastErr == nil {
		return nil, "empty", nil
	}
	return nil, "exhausted", lastErr
}

type endpoint struct {
	label string
	url   string
}

// publisherListEndpoints returns the ordered list of URLs to try for a given
// publisher. The unscoped global catalog endpoint is always last because it
// works without project quota but lists every publisher model (we still want
// to honor any project-scoped restrictions when possible).
func (vc *VertexClient) publisherListEndpoints(publisher string) []endpoint {
	var endpoints []endpoint
	if vc.projectID != "" && vc.location != "" {
		if vc.location == "global" {
			endpoints = append(endpoints, endpoint{
				label: "project-global",
				url: fmt.Sprintf(
					"https://aiplatform.googleapis.com/v1beta1/projects/%s/locations/global/publishers/%s/models",
					vc.projectID, publisher),
			})
		} else {
			endpoints = append(endpoints, endpoint{
				label: "project-regional",
				url: fmt.Sprintf(
					"https://%s-aiplatform.googleapis.com/v1beta1/projects/%s/locations/%s/publishers/%s/models",
					vc.location, vc.projectID, vc.location, publisher),
			})
		}

		// us-central1 has the broadest publisher coverage; use it as fallback
		// whenever the configured location isn't already us-central1.
		if vc.location != "us-central1" {
			endpoints = append(endpoints, endpoint{
				label: "project-us-central1",
				url: fmt.Sprintf(
					"https://us-central1-aiplatform.googleapis.com/v1beta1/projects/%s/locations/us-central1/publishers/%s/models",
					vc.projectID, publisher),
			})
		}
	}
	// Global catalog: lists every publisher model, regardless of project.
	endpoints = append(endpoints, endpoint{
		label: "global-catalog",
		url:   fmt.Sprintf("https://us-central1-aiplatform.googleapis.com/v1beta1/publishers/%s/models", publisher),
	})
	return endpoints
}

// getModelsFromEndpoint performs a single GET against a publisher-listing URL
// and decodes the result into genai.Model objects.
func (vc *VertexClient) getModelsFromEndpoint(ctx context.Context, url string) ([]*genai.Model, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if vc.projectID != "" {
		req.Header.Set("x-goog-user-project", vc.projectID)
	}
	resp, err := vc.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		PublisherModels []map[string]interface{} `json:"publisherModels"`
		Models          []map[string]interface{} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	rawEntries := append([]map[string]interface{}{}, payload.PublisherModels...)
	rawEntries = append(rawEntries, payload.Models...)

	var out []*genai.Model
	for _, m := range rawEntries {
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		out = append(out, &genai.Model{
			Name:             name,
			SupportedActions: extractSupportedActions(m),
		})
	}
	return out, nil
}

// extractSupportedActions tries to read the "supportedActions" or
// "launchStage"/"supportedGenerationMethods" hints from a raw model payload.
// When the field is absent we leave SupportedActions empty and let
// isChatModel() apply heuristic fall-backs.
func extractSupportedActions(m map[string]interface{}) []string {
	if v, ok := m["supportedActions"].([]interface{}); ok {
		var acts []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				acts = append(acts, s)
			}
		}
		return acts
	}
	if v, ok := m["supportedGenerationMethods"].([]interface{}); ok {
		var acts []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				acts = append(acts, s)
			}
		}
		return acts
	}
	return nil
}

// listDeployedModels queries the genai SDK for non-base (tuned / deployed)
// models that the project has access to.
func (vc *VertexClient) listDeployedModels(ctx context.Context) ([]*genai.Model, error) {
	queryBaseFalse := false
	config := &genai.ListModelsConfig{QueryBase: &queryBaseFalse}

	var collected []*genai.Model
	pager, err := vc.client.Models.List(ctx, config)
	if err != nil {
		return nil, err
	}
	collected = append(collected, pager.Items...)
	for pager.NextPageToken != "" {
		pager, err = vc.client.Models.List(ctx, &genai.ListModelsConfig{
			PageToken: pager.NextPageToken,
			QueryBase: &queryBaseFalse,
		})
		if err != nil {
			return collected, err
		}
		collected = append(collected, pager.Items...)
	}
	return collected, nil
}

// lastSegment returns the final '/'-delimited segment of a resource path.
func lastSegment(name string) string {
	if name == "" {
		return ""
	}
	idx := strings.LastIndex(name, "/")
	if idx < 0 {
		return name
	}
	return name[idx+1:]
}

func isChatModel(m *genai.Model) bool {
	if len(m.SupportedActions) > 0 {
		for _, action := range m.SupportedActions {
			a := strings.ToLower(action)
			if a == "generatecontent" || a == "predict" || a == "streamgeneratecontent" || a == "streamrawpredict" {
				return true
			}
		}
		return false
	}
	if m.OutputTokenLimit > 0 {
		return true
	}
	// Fall-back heuristic: surface anything that isn't obviously an embedding /
	// image / safety / vision-encoder model.
	name := strings.ToLower(m.Name)
	exclude := []string{"embed", "embedding", "imagen", "imagegeneration", "veo", "moderation", "tts", "stt", "speech"}
	for _, ex := range exclude {
		if strings.Contains(name, ex) {
			return false
		}
	}
	return true
}

// MapRole maps Ollama roles to Vertex AI roles.
func MapRole(role string) string {
	role = strings.ToLower(role)
	switch role {
	case "assistant":
		return genai.RoleModel
	case "system":
		return ""
	default:
		return genai.RoleUser
	}
}
