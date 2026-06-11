package provider

import (
	"go.f0o.dev/cline-vertex-gw/pkg/pipeline"
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/genai"
)

func TestParsePublisher(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantPublisher string
		wantModelID   string
	}{
		// already-qualified resource paths
		{"qualified anthropic", "publishers/anthropic/models/claude-opus-4-7", "anthropic", "claude-opus-4-7"},
		{"qualified google", "publishers/google/models/gemini-2.0-flash", "google", "gemini-2.0-flash"},
		{"qualified meta", "publishers/meta/models/llama-3.3-70b-instruct-maas", "meta", "llama-3.3-70b-instruct-maas"},

		// prefixed short ids
		{"prefixed anthropic", "anthropic/claude-opus-4-7", "anthropic", "claude-opus-4-7"},
		{"prefixed mistralai", "mistralai/mistral-large-2411", "mistralai", "mistral-large-2411"},
		{"prefixed deepseek", "deepseek-ai/deepseek-v3", "deepseek-ai", "deepseek-v3"},
		{"prefixed xai", "xai/grok-2", "xai", "grok-2"},
		{"prefixed minimax", "minimax/abab6.5", "minimax", "abab6.5"},
		{"prefixed moonshotai", "moonshotai/kimi-latest", "moonshotai", "kimi-latest"},
		{"prefixed zhipuai", "zhipuai/glm-4", "zhipuai", "glm-4"},
		{"prefixed openai", "openai/gpt-4o", "openai", "gpt-4o"},

		// prefixed short ids via aliases
		{"prefixed deepseek alias", "deepseek/deepseek-v3", "deepseek-ai", "deepseek-v3"},
		{"prefixed glm alias", "glm/glm-4", "zhipuai", "glm-4"},
		{"prefixed zhipu alias", "zhipu/glm-4", "zhipuai", "glm-4"},
		{"prefixed moonshot alias", "moonshot/kimi-latest", "moonshotai", "kimi-latest"},

		// bare short ids — heuristic by substring
		{"bare claude", "claude-opus-4-7", "anthropic", "claude-opus-4-7"},
		{"bare llama", "llama-3.3-70b-instruct", "meta", "llama-3.3-70b-instruct"},
		{"bare mistral", "mistral-large-2411", "mistralai", "mistral-large-2411"},
		{"bare mixtral", "mixtral-8x22b", "mistralai", "mixtral-8x22b"},
		{"bare codestral", "codestral-2405", "mistralai", "codestral-2405"},
		{"bare jamba", "jamba-1.5-large", "ai21", "jamba-1.5-large"},
		{"bare command", "cohere-command-r-plus", "cohere", "cohere-command-r-plus"},
		{"bare deepseek", "deepseek-v3", "deepseek-ai", "deepseek-v3"},
		{"bare qwen", "qwen-2.5-72b", "qwen", "qwen-2.5-72b"},
		{"bare grok", "grok-2", "xai", "grok-2"},
		{"bare minimax", "minimax-abab6.5", "minimax", "minimax-abab6.5"},
		{"bare abab", "abab6.5", "minimax", "abab6.5"},
		{"bare moonshot", "moonshot-kimi", "moonshotai", "moonshot-kimi"},
		{"bare kimi", "kimi-latest", "moonshotai", "kimi-latest"},
		{"bare glm", "glm-4", "zhipuai", "glm-4"},
		{"bare zhipu", "zhipu-model", "zhipuai", "zhipu-model"},
		{"bare gpt", "gpt-4o", "openai", "gpt-4o"},

		// defaults / unknown → google
		{"bare gemini", "gemini-2.0-flash", "google", "gemini-2.0-flash"},
		{"unknown", "some-random-model", "google", "some-random-model"},

		// Versioned id with dot in head: the dot-check skips the explicit
		// publisher-prefix branch, but the substring heuristic still routes
		// "claude" → anthropic. modelID is the full string (no split).
		{"dotted head falls through to heuristic", "1.5/claude-opus", "anthropic", "1.5/claude-opus"},
		// Truly unknown id with dotted head → default "google".
		{"dotted head unknown", "1.5/foo-bar", "google", "1.5/foo-bar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPub, gotID := ParsePublisher(tt.input)
			if gotPub != tt.wantPublisher || gotID != tt.wantModelID {
				t.Errorf("ParsePublisher(%q) = (%q, %q); want (%q, %q)",
					tt.input, gotPub, gotID, tt.wantPublisher, tt.wantModelID)
			}
		})
	}
}

func TestFormatModelName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"publishers/anthropic/models/claude-opus-4-7", "publishers/anthropic/models/claude-opus-4-7"},
		{"gemini-2.0-flash", "gemini-2.0-flash"},
		{"claude-opus-4-7", "publishers/anthropic/models/claude-opus-4-7"},
		{"llama-3.3-70b-instruct", "publishers/meta/models/llama-3.3-70b-instruct"},
		{"mistral-large-2411", "publishers/mistralai/models/mistral-large-2411"},
	}
	for _, tt := range tests {
		if got := FormatModelName(tt.in); got != tt.want {
			t.Errorf("FormatModelName(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestMapRole(t *testing.T) {
	tests := []struct{ in, want string }{
		{"user", "user"},
		{"USER", "user"},
		{"assistant", "model"},
		{"Assistant", "model"},
		{"system", ""},   // system is hoisted out separately
		{"tool", "user"}, // unknown → user
		{"", "user"},
	}
	for _, tt := range tests {
		if got := MapRole(tt.in); got != tt.want {
			t.Errorf("MapRole(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestPublisherEndpoint(t *testing.T) {
	tests := []struct {
		name      string
		location  string
		publisher string
		model     string
		method    string
		wantHost  string
	}{
		{"global uses bare host", "global", "anthropic", "claude-opus-4-7", "rawPredict",
			"https://aiplatform.googleapis.com"},
		{"regional uses prefixed host", "us-east5", "anthropic", "claude-opus-4-7", "streamRawPredict",
			"https://us-east5-aiplatform.googleapis.com"},
		{"empty location uses bare host", "", "meta", "llama-3.3-70b-instruct-maas", "rawPredict",
			"https://aiplatform.googleapis.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vc := &VertexClient{projectID: "my-proj", location: tt.location}
			got := vc.publisherEndpoint(tt.publisher, tt.model, tt.method)
			wantPrefix := tt.wantHost + "/v1/projects/my-proj/locations/" + tt.location + "/publishers/" + tt.publisher + "/models/" + tt.model + ":" + tt.method
			if got != wantPrefix {
				t.Errorf("publisherEndpoint = %q; want %q", got, wantPrefix)
			}
		})
	}
}

func TestDebugLogPayload(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)

	oldLogger := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(oldLogger)

	ctx := context.WithValue(context.Background(), ContextKeyReqID, "test-req-123")
	ctx = context.WithValue(ctx, ContextKeyRoute, "test-route")

	payload := map[string]string{"foo": "bar"}

	// Test 1: Suppressed by default (dumpPayloads = false)
	oldDump := dumpPayloads
	dumpPayloads = false
	buf.Reset()

	DebugLogPayload(ctx, "test_step", payload)

	if buf.Len() > 0 {
		t.Errorf("expected payload log to be suppressed, but got: %q", buf.String())
	}

	// Test 2: Enabled (dumpPayloads = true)
	dumpPayloads = true
	buf.Reset()

	DebugLogPayload(ctx, "test_step", payload)

	logOutput := buf.String()
	if logOutput == "" {
		t.Errorf("expected payload log to be emitted, but got empty buffer")
	} else {
		if !strings.Contains(logOutput, "test_step") {
			t.Errorf("expected log to contain step name 'test_step', got %q", logOutput)
		}
		if !strings.Contains(logOutput, "test-req-123") {
			t.Errorf("expected log to contain request ID 'test-req-123', got %q", logOutput)
		}
		if !strings.Contains(logOutput, "test-route") {
			t.Errorf("expected log to contain route 'test-route', got %q", logOutput)
		}
		if !strings.Contains(logOutput, "foo") || !strings.Contains(logOutput, "bar") {
			t.Errorf("expected log to contain JSON payload, got %q", logOutput)
		}
	}

	// Restore original state
	dumpPayloads = oldDump
}

type mockRoundTripper struct {
	roundTripFunc func(req *http.Request) (*http.Response, error)
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.roundTripFunc(req)
}

func TestRoutingTierRoundTripper(t *testing.T) {
	tests := []struct {
		name          string
		tierInCtx     any
		wantHeaderVal string
		expectHeader  bool
	}{
		{
			name:          "routing tier present",
			tierInCtx:     "priority",
			wantHeaderVal: "priority",
			expectHeader:  true,
		},
		{
			name:          "routing tier flex",
			tierInCtx:     "flex",
			wantHeaderVal: "flex",
			expectHeader:  true,
		},
		{
			name:         "routing tier empty",
			tierInCtx:    "",
			expectHeader: false,
		},
		{
			name:         "routing tier missing from context",
			tierInCtx:    nil,
			expectHeader: false,
		},
		{
			name:         "routing tier wrong type",
			tierInCtx:    123,
			expectHeader: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.tierInCtx != nil {
				ctx = context.WithValue(ctx, ContextKeyRoutingTier, tt.tierInCtx)
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/test", nil)
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}

			// Mock round tripper that captures the forwarded request headers
			var capturedHeader http.Header
			mockRT := &mockRoundTripper{
				roundTripFunc: func(r *http.Request) (*http.Response, error) {
					capturedHeader = r.Header
					return &http.Response{StatusCode: http.StatusOK}, nil
				},
			}

			rt := &routingTierRoundTripper{underlying: mockRT}
			_, err = rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip failed: %v", err)
			}

			val := capturedHeader.Get("X-Vertex-AI-Routing-Tier")
			valReqType := capturedHeader.Get("X-Vertex-AI-LLM-Request-Type")
			valSharedType := capturedHeader.Get("X-Vertex-AI-LLM-Shared-Request-Type")

			if tt.expectHeader {
				if val != tt.wantHeaderVal {
					t.Errorf("X-Vertex-AI-Routing-Tier = %q; want %q", val, tt.wantHeaderVal)
				}
				if tt.wantHeaderVal == "flex" || tt.wantHeaderVal == "priority" {
					if valReqType != "shared" {
						t.Errorf("X-Vertex-AI-LLM-Request-Type = %q; want %q", valReqType, "shared")
					}
					if valSharedType != tt.wantHeaderVal {
						t.Errorf("X-Vertex-AI-LLM-Shared-Request-Type = %q; want %q", valSharedType, tt.wantHeaderVal)
					}
				}
			} else {
				if val != "" {
					t.Errorf("expected no X-Vertex-AI-Routing-Tier header, but got %q", val)
				}
				if valReqType != "" {
					t.Errorf("expected no X-Vertex-AI-LLM-Request-Type header, but got %q", valReqType)
				}
				if valSharedType != "" {
					t.Errorf("expected no X-Vertex-AI-LLM-Shared-Request-Type header, but got %q", valSharedType)
				}
			}
		})
	}
}

func TestGetConfig_SearchGrounding(t *testing.T) {
	vc := &VertexClient{}

	tests := []struct {
		name               string
		envGrounding       string
		envThreshold       string
		wantSearch         bool
		wantEnterprise     bool
		wantThreshold      float32
		wantThresholdIsSet bool
	}{
		{
			name:         "no grounding",
			envGrounding: "",
			wantSearch:   false,
		},
		{
			name:         "google search retrieval with no threshold",
			envGrounding: "google_search_retrieval",
			wantSearch:   true,
		},
		{
			name:               "google search with threshold",
			envGrounding:       "google_search",
			envThreshold:       "0.5",
			wantSearch:         true,
			wantThreshold:      0.5,
			wantThresholdIsSet: true,
		},
		{
			name:           "enterprise web search",
			envGrounding:   "enterprise_web_search",
			wantSearch:     true,
			wantEnterprise: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GW_GEMINI_SEARCH_GROUNDING", tt.envGrounding)
			if tt.envThreshold != "" {
				t.Setenv("GW_GEMINI_SEARCH_THRESHOLD", tt.envThreshold)
			} else {
				t.Setenv("GW_GEMINI_SEARCH_THRESHOLD", "")
			}

			config := vc.GetConfig("gemini-1.5-pro", "", nil)
			if !tt.wantSearch {
				if len(config.Tools) > 0 {
					t.Errorf("expected no tools, got %d", len(config.Tools))
				}
				return
			}

			if len(config.Tools) != 1 {
				t.Fatalf("expected exactly 1 tool, got %d", len(config.Tools))
			}

			tool := config.Tools[0]
			if tt.wantEnterprise {
				if tool.EnterpriseWebSearch == nil {
					t.Error("expected EnterpriseWebSearch tool, got nil")
				}
			} else {
				if tool.GoogleSearchRetrieval == nil {
					t.Fatalf("expected GoogleSearchRetrieval tool, got nil")
				}
				if tt.wantThresholdIsSet {
					config := tool.GoogleSearchRetrieval.DynamicRetrievalConfig
					if config == nil {
						t.Fatal("expected DynamicRetrievalConfig, got nil")
					}
					if config.DynamicThreshold == nil {
						t.Fatal("expected DynamicThreshold, got nil")
					}
					if *config.DynamicThreshold != tt.wantThreshold {
						t.Errorf("DynamicThreshold = %v, want %v", *config.DynamicThreshold, tt.wantThreshold)
					}
					if config.Mode != "MODE_DYNAMIC" {
						t.Errorf("Mode = %q, want MODE_DYNAMIC", config.Mode)
					}
				}
			}
		})
	}
}

func TestGetConfig_SearchGrounding_NewGemini(t *testing.T) {
	vc := &VertexClient{}

	t.Setenv("GW_GEMINI_SEARCH_GROUNDING", "google_search")
	t.Setenv("GW_GEMINI_SEARCH_THRESHOLD", "")

	// On newer models like gemini-3.5-flash, the field should be GoogleSearch and NOT GoogleSearchRetrieval
	config := vc.GetConfig("gemini-3.5-flash", "", nil)
	if len(config.Tools) != 1 {
		t.Fatalf("expected exactly 1 tool, got %d", len(config.Tools))
	}

	tool := config.Tools[0]
	if tool.GoogleSearch == nil {
		t.Error("expected GoogleSearch tool, got nil")
	}
	if tool.GoogleSearchRetrieval != nil {
		t.Error("expected GoogleSearchRetrieval tool to be nil for newer gemini models")
	}

	// Test mapping client-supplied tools
	opts := &pipeline.GenerationOptions{
		Tools: []*genai.Tool{
			{
				GoogleSearchRetrieval: &genai.GoogleSearchRetrieval{},
			},
		},
	}
	config2 := vc.GetConfig("gemini-3.5-flash", "", opts)
	// We expect the original GoogleSearchRetrieval in opts.Tools to be converted to GoogleSearch in config2.Tools
	// Since GW_GEMINI_SEARCH_GROUNDING is still set to google_search, we will get 2 tools in config2.Tools:
	// 1 mapped from opts.Tools, and 1 injected globally.
	if len(config2.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(config2.Tools))
	}
	for i, tool := range config2.Tools {
		if tool.GoogleSearch == nil {
			t.Errorf("tool %d: expected GoogleSearch, got nil", i)
		}
		if tool.GoogleSearchRetrieval != nil {
			t.Errorf("tool %d: expected GoogleSearchRetrieval to be nil, got non-nil", i)
		}
	}
}
