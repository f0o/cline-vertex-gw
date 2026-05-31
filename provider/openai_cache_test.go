package provider

import (
	"encoding/json"
	"testing"
)

// TestOpenAIUsage_CachedTokens verifies that the implicit-cache read count is
// parsed from `usage.prompt_tokens_details.cached_tokens` (the shape DeepSeek
// and other OpenAI-compatible providers report), and that its absence yields 0.
func TestOpenAIUsage_CachedTokens(t *testing.T) {
	t.Run("with cached tokens", func(t *testing.T) {
		raw := `{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"prompt_tokens_details":{"cached_tokens":768}}`
		var u openaiUsage
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if u.PromptTokens != 1000 {
			t.Errorf("PromptTokens = %d; want 1000", u.PromptTokens)
		}
		if got := u.cachedTokens(); got != 768 {
			t.Errorf("cachedTokens() = %d; want 768", got)
		}
	})

	t.Run("without details", func(t *testing.T) {
		raw := `{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050}`
		var u openaiUsage
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := u.cachedTokens(); got != 0 {
			t.Errorf("cachedTokens() = %d; want 0 when details absent", got)
		}
	})
}
