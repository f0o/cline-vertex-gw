package provider

import (
	"fmt"
	"strings"

	"google.golang.org/genai"
)

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

// adapterKind classifies which backend translation path a publisher uses.
type adapterKind int

const (
	adapterGoogle       adapterKind = iota // genai SDK (Gemini / Gemma)
	adapterAnthropic                       // Anthropic Messages API
	adapterCohere                          // Cohere /chat API
	adapterOpenAICompat                    // shared OpenAI-compatible adapter
)

// publisherInfo is the single source of truth for one Vertex publisher: how to
// recognize it from a prefixed model id, which backend adapter handles it, and
// whether it participates in /api/tags discovery.
type publisherInfo struct {
	kind         adapterKind
	discoverable bool // included in supportedPublishers for ListModels fan-out
}

// publisherRegistry is the ONE place that enumerates every publisher namespace
// the gateway knows about. ParsePublisher, the Generate/GenerateStream
// dispatch, openai-compat classification, and the discovery list all derive
// from this map so adding a publisher is a single-entry change.
var publisherRegistry = map[string]publisherInfo{
	"google":      {kind: adapterGoogle, discoverable: true},
	"anthropic":   {kind: adapterAnthropic, discoverable: true},
	"cohere":      {kind: adapterCohere, discoverable: true},
	"meta":        {kind: adapterOpenAICompat, discoverable: true}, // Llama 3.x / 4 (MaaS)
	"mistralai":   {kind: adapterOpenAICompat, discoverable: true}, // Mistral Large, Codestral, Mixtral
	"ai21":        {kind: adapterOpenAICompat, discoverable: true}, // Jamba 1.5 / 2.x
	"deepseek-ai": {kind: adapterOpenAICompat, discoverable: true}, // DeepSeek
	"qwen":        {kind: adapterOpenAICompat, discoverable: true}, // Qwen
	"nvidia":      {kind: adapterOpenAICompat, discoverable: true}, // Nvidia-hosted MaaS models
	"moonshotai":  {kind: adapterOpenAICompat, discoverable: true}, // Moonshot
	"xai":         {kind: adapterOpenAICompat, discoverable: true}, // xAI (Grok)
	"minimax":     {kind: adapterOpenAICompat, discoverable: true}, // MiniMax
	"zhipuai":     {kind: adapterOpenAICompat, discoverable: true}, // GLM / Zhipu AI
	"openai":      {kind: adapterOpenAICompat, discoverable: true}, // OpenAI models on Vertex
}

// publisherAliases maps user-facing or common publisher names to their
// canonical Vertex AI Model Garden namespaces.
var publisherAliases = map[string]string{
	"glm":      "zhipuai",
	"zhipu":    "zhipuai",
	"deepseek": "deepseek-ai",
	"moonshot": "moonshotai",
}

// publisherSubstringHints maps a case-insensitive substring of a bare model id
// to its publisher, used when the id carries no explicit "publisher/" prefix.
// Order matters only for documentation; each model matches at most one family.
var publisherSubstringHints = []struct {
	substr    string
	publisher string
}{
	{"claude", "anthropic"},
	{"llama", "meta"},
	{"mistral", "mistralai"},
	{"mixtral", "mistralai"},
	{"codestral", "mistralai"},
	{"jamba", "ai21"},
	{"command", "cohere"},
	{"deepseek", "deepseek-ai"},
	{"qwen", "qwen"},
	{"grok", "xai"},
	{"minimax", "minimax"},
	{"abab", "minimax"},
	{"moonshot", "moonshotai"},
	{"kimi", "moonshotai"},
	{"glm", "zhipuai"},
	{"zhipu", "zhipuai"},
	{"gpt", "openai"},
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
	// "anthropic/claude-opus-4-7" style: a known publisher prefix before the
	// first slash (and no dot, which would indicate a versioned bare id).
	if idx := strings.Index(model, "/"); idx > 0 && !strings.Contains(model[:idx], ".") {
		head := model[:idx]
		if isKnownPublisher(head) {
			return head, model[idx+1:]
		}
		if canonical, ok := publisherAliases[strings.ToLower(head)]; ok {
			return canonical, model[idx+1:]
		}
	}

	lower := strings.ToLower(model)
	for _, hint := range publisherSubstringHints {
		if strings.Contains(lower, hint.substr) {
			return hint.publisher, model
		}
	}
	return "google", model
}

// isKnownPublisher reports whether name is a publisher namespace the gateway
// recognizes (has a registry entry).
func isKnownPublisher(name string) bool {
	_, ok := publisherRegistry[name]
	return ok
}

// PublisherKind returns the adapterKind of a publisher namespace, if recognized.
func PublisherKind(publisher string) (adapterKind, bool) {
	info, ok := publisherRegistry[publisher]
	if !ok {
		return 0, false
	}
	return info.kind, true
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
