package provider

import "testing"

func TestParsePublisher(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantPublisher string
		wantModelID   string
	}{
		// already-qualified resource paths
		{"qualified anthropic", "publishers/anthropic/models/claude-opus-4-7", "anthropic", "claude-opus-4-7"},
		{"qualified google",    "publishers/google/models/gemini-2.0-flash",   "google",    "gemini-2.0-flash"},
		{"qualified meta",      "publishers/meta/models/llama-3.3-70b-instruct-maas", "meta", "llama-3.3-70b-instruct-maas"},

		// prefixed short ids
		{"prefixed anthropic", "anthropic/claude-opus-4-7",      "anthropic",   "claude-opus-4-7"},
		{"prefixed mistralai", "mistralai/mistral-large-2411",   "mistralai",   "mistral-large-2411"},
		{"prefixed deepseek",  "deepseek-ai/deepseek-v3",        "deepseek-ai", "deepseek-v3"},

		// bare short ids — heuristic by substring
		{"bare claude",   "claude-opus-4-7",            "anthropic",   "claude-opus-4-7"},
		{"bare llama",    "llama-3.3-70b-instruct",     "meta",        "llama-3.3-70b-instruct"},
		{"bare mistral",  "mistral-large-2411",         "mistralai",   "mistral-large-2411"},
		{"bare mixtral",  "mixtral-8x22b",              "mistralai",   "mixtral-8x22b"},
		{"bare codestral","codestral-2405",             "mistralai",   "codestral-2405"},
		{"bare jamba",    "jamba-1.5-large",            "ai21",        "jamba-1.5-large"},
		{"bare command",  "cohere-command-r-plus",      "cohere",      "cohere-command-r-plus"},
		{"bare deepseek", "deepseek-v3",                "deepseek-ai", "deepseek-v3"},
		{"bare qwen",     "qwen-2.5-72b",               "qwen",        "qwen-2.5-72b"},

		// defaults / unknown → google
		{"bare gemini",   "gemini-2.0-flash",           "google",      "gemini-2.0-flash"},
		{"unknown",       "some-random-model",          "google",      "some-random-model"},

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
		{"gemini-2.0-flash",                            "gemini-2.0-flash"},
		{"claude-opus-4-7",                             "publishers/anthropic/models/claude-opus-4-7"},
		{"llama-3.3-70b-instruct",                      "publishers/meta/models/llama-3.3-70b-instruct"},
		{"mistral-large-2411",                          "publishers/mistralai/models/mistral-large-2411"},
	}
	for _, tt := range tests {
		if got := FormatModelName(tt.in); got != tt.want {
			t.Errorf("FormatModelName(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestMapRole(t *testing.T) {
	tests := []struct{ in, want string }{
		{"user",      "user"},
		{"USER",      "user"},
		{"assistant", "model"},
		{"Assistant", "model"},
		{"system",    ""},     // system is hoisted out separately
		{"tool",      "user"}, // unknown → user
		{"",          "user"},
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