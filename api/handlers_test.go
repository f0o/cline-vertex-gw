package api

import (
	"strings"
	"testing"
)

func TestBuildContents(t *testing.T) {
	t.Run("system messages are concatenated and hoisted", func(t *testing.T) {
		msgs := []Message{
			{Role: "system", Content: "be brief"},
			{Role: "system", Content: "use markdown"},
			{Role: "user", Content: "hi"},
		}
		contents, system, _ := buildContents(msgs)
		if !strings.Contains(system, "be brief") || !strings.Contains(system, "use markdown") {
			t.Errorf("system prompt missing parts: %q", system)
		}
		if len(contents) != 1 {
			t.Fatalf("want 1 content block, got %d", len(contents))
		}
		if contents[0].Role != "user" {
			t.Errorf("first content role = %q; want user", contents[0].Role)
		}
	})

	t.Run("consecutive same-role messages merge", func(t *testing.T) {
		msgs := []Message{
			{Role: "user", Content: "a"},
			{Role: "user", Content: "b"},
			{Role: "assistant", Content: "x"},
			{Role: "assistant", Content: "y"},
		}
		contents, _, _ := buildContents(msgs)
		if len(contents) != 2 {
			t.Fatalf("want 2 merged blocks, got %d", len(contents))
		}
		if len(contents[0].Parts) != 2 || contents[0].Parts[0].Text != "a" || contents[0].Parts[1].Text != "b" {
			t.Errorf("user block parts wrong: %+v", contents[0].Parts)
		}
		if len(contents[1].Parts) != 2 || contents[1].Parts[0].Text != "x" || contents[1].Parts[1].Text != "y" {
			t.Errorf("assistant block parts wrong: %+v", contents[1].Parts)
		}
	})

	t.Run("alternating roles produce separate blocks", func(t *testing.T) {
		msgs := []Message{
			{Role: "user", Content: "a"},
			{Role: "assistant", Content: "b"},
			{Role: "user", Content: "c"},
		}
		contents, _, _ := buildContents(msgs)
		if len(contents) != 3 {
			t.Fatalf("want 3 blocks, got %d", len(contents))
		}
	})

	t.Run("empty input", func(t *testing.T) {
		contents, system, _ := buildContents(nil)
		if len(contents) != 0 || system != "" {
			t.Errorf("want zero values; got contents=%d system=%q", len(contents), system)
		}
	})
}

func TestDoneReason(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "stop"},
		{"STOP", "stop"},
		{"FINISH_REASON_STOP", "stop"},
		{"MAX_TOKENS", "length"},
		{"FINISH_REASON_MAX_TOKENS", "length"},
		{"SAFETY", "safety"},
		{"FINISH_REASON_SAFETY", "safety"},
		{"RECITATION", "recitation"},
		{"FINISH_REASON_RECITATION", "recitation"},
		{"WEIRD", "weird"}, // pass-through lowercased
	}
	for _, tt := range tests {
		if got := doneReason(tt.in); got != tt.want {
			t.Errorf("doneReason(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestFamilyFromName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"gemini-2.0-flash", "gemini"},
		{"gemma-2-9b", "gemma"},
		{"claude-opus-4-7", "claude"},
		{"llama-3.3-70b-instruct-maas", "llama"},
		{"mistral-large-2411", "mistral"},
		{"mixtral-8x22b", "mistral"},
		{"codestral-2405", "mistral"},
		{"jamba-1.5-large", "jamba"},
		{"cohere-command-r-plus", "command"},
		{"deepseek-v3", "deepseek"},
		{"qwen-2.5-72b", "qwen"},
		{"some-unknown-model", "vertex"},
	}
	for _, tt := range tests {
		if got := familyFromName(tt.in); got != tt.want {
			t.Errorf("familyFromName(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}