package provider

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

func mkImagePart(mime string) *genai.Part {
	return &genai.Part{InlineData: &genai.Blob{MIMEType: mime, Data: []byte{0x89, 0x50, 0x4E, 0x47}}}
}

func TestHasInlineImageParts(t *testing.T) {
	cases := []struct {
		name string
		in   []*genai.Content
		want bool
	}{
		{"nil", nil, false},
		{"text only", []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}}}, false},
		{
			"image present",
			[]*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{mkImagePart("image/png")}}},
			true,
		},
		{
			"image mixed with text",
			[]*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}, mkImagePart("image/png")}}},
			true,
		},
		{
			"empty inline data ignored",
			[]*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{InlineData: &genai.Blob{MIMEType: "image/png"}}}}},
			// hasInlineImageParts doesn't check the data length — the
			// part exists so it counts. Vision gate decides based on
			// presence, not byte count.
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasInlineImageParts(tc.in); got != tc.want {
				t.Errorf("hasInlineImageParts = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestPublisherSupportsVision(t *testing.T) {
	cases := []struct {
		publisher string
		modelID   string
		want      bool
	}{
		// google: liberal — everything Gemini/Gemma-3 + unknown defaults to yes
		{"google", "gemini-2.0-flash", true},
		{"google", "gemini-1.5-pro", true},
		{"google", "gemma-3-27b-it", true},
		{"google", "future-model", true}, // default-permissive on google
		// anthropic: all Claude 3+
		{"anthropic", "claude-3-haiku", true},
		{"anthropic", "claude-3-5-sonnet", true},
		{"anthropic", "claude-opus-4-7", true},
		{"anthropic", "claude-sonnet-4-thinking", true},
		// meta: only vision SKUs
		{"meta", "llama-3.2-90b-vision-instruct-maas", true},
		{"meta", "llama-3.1-405b-instruct-maas", false},
		{"meta", "llama-3.3-70b-instruct-maas", false},
		{"meta", "llama-4-scout", true}, // llama-4 multimodal-by-default
		// mistralai: pixtral only
		{"mistralai", "pixtral-12b", true},
		{"mistralai", "mistral-large-2411", false},
		{"mistralai", "codestral-2501", false},
		// qwen
		{"qwen", "qwen2-vl-72b", true},
		{"qwen", "qwen2.5-vl-7b", true},
		{"qwen", "qwen2.5-72b", false},
		// nvidia
		{"nvidia", "llama-3.2-nv-vision-90b", true},
		{"nvidia", "nemotron-4-340b", false},
		// no-vision publishers
		{"cohere", "command-r-plus", false},
		{"cohere", "command-r-08-2024", false},
		{"ai21", "jamba-1.5-large", false},
		{"deepseek-ai", "deepseek-v3", false},
		// unknown publisher: deny by default
		{"newpublisher", "newmodel-1", false},
	}
	for _, tc := range cases {
		t.Run(tc.publisher+"/"+tc.modelID, func(t *testing.T) {
			if got := publisherSupportsVision(tc.publisher, tc.modelID); got != tc.want {
				t.Errorf("publisherSupportsVision(%q, %q) = %v; want %v",
					tc.publisher, tc.modelID, got, tc.want)
			}
		})
	}
}

func TestCheckVisionSupport_NoImagesIsAlwaysOK(t *testing.T) {
	// Even Cohere (which doesn't support vision) shouldn't 400 a text-only
	// request — the gate is image-conditional.
	in := []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}}}
	for _, model := range []string{"gemini-2.0-flash", "command-r-plus", "claude-3-haiku", "llama-3.1-405b-instruct-maas"} {
		if err := CheckVisionSupport(model, in); err != nil {
			t.Errorf("text-only request rejected for model=%q: %v", model, err)
		}
	}
}

func TestCheckVisionSupport_AllowsCapableModels(t *testing.T) {
	in := []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{
		{Text: "what is this"},
		mkImagePart("image/png"),
	}}}
	for _, model := range []string{
		"gemini-2.0-flash",
		"claude-3-5-sonnet",
		"claude-opus-4-7",
		"llama-3.2-90b-vision-instruct-maas",
		"pixtral-12b",
		"qwen2-vl-72b",
	} {
		if err := CheckVisionSupport(model, in); err != nil {
			t.Errorf("image-bearing request rejected for vision-capable model=%q: %v", model, err)
		}
	}
}

func TestCheckVisionSupport_RejectsTextOnlyModels(t *testing.T) {
	in := []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{
		mkImagePart("image/png"),
	}}}
	for _, model := range []string{
		"command-r-plus",
		"command-r-08-2024",
		"jamba-1.5-large",
		"deepseek-v3",
		"llama-3.1-405b-instruct-maas",
		"llama-3.3-70b-instruct-maas",
		"mistral-large-2411",
		"codestral-2501",
	} {
		t.Run(model, func(t *testing.T) {
			err := CheckVisionSupport(model, in)
			if err == nil {
				t.Fatalf("text-only model %q should have rejected image input", model)
			}
			// Error must name the model and suggest alternatives.
			if !strings.Contains(err.Error(), model) {
				t.Errorf("error does not name the model: %v", err)
			}
			if !strings.Contains(err.Error(), "vision-capable") {
				t.Errorf("error does not suggest alternatives: %v", err)
			}
		})
	}
}
