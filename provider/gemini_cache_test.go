package provider

import (
	"context"
	"testing"
	"time"

	"google.golang.org/genai"
)

// TestGeminiCacheRegistry_StoreLookup verifies a stored entry is returned while
// valid and dropped once it is within the safety margin of expiry.
func TestGeminiCacheRegistry_StoreLookup(t *testing.T) {
	r := &geminiCacheRegistry{m: map[string]geminiCacheEntry{}}
	now := time.Now()

	r.store("k1", "projects/p/locations/l/cachedContents/abc", now.Add(time.Hour))

	if got := r.lookup("k1", now); got != "projects/p/locations/l/cachedContents/abc" {
		t.Errorf("lookup hit = %q; want the stored resource name", got)
	}
	if got := r.lookup("missing", now); got != "" {
		t.Errorf("lookup miss = %q; want empty", got)
	}
}

// TestGeminiCacheRegistry_ExpiryEviction verifies that an entry within the
// safety margin of (or past) its expiry is treated as a miss AND removed.
func TestGeminiCacheRegistry_ExpiryEviction(t *testing.T) {
	r := &geminiCacheRegistry{m: map[string]geminiCacheEntry{}}
	now := time.Now()

	// Expires in 5s, but the safety margin is 15s => should be treated as
	// expired right now.
	r.store("soon", "res-soon", now.Add(5*time.Second))
	if got := r.lookup("soon", now); got != "" {
		t.Errorf("lookup of near-expiry entry = %q; want empty (safety margin)", got)
	}
	// And it should have been evicted.
	if _, ok := r.m["soon"]; ok {
		t.Errorf("near-expiry entry was not evicted from the map")
	}

	// A comfortably-future entry is still valid.
	r.store("later", "res-later", now.Add(time.Minute))
	if got := r.lookup("later", now); got != "res-later" {
		t.Errorf("lookup of valid entry = %q; want res-later", got)
	}
}

// TestGeminiCacheKey_Stability verifies the cache key is identical for the same
// model+system+tools and differs when any component changes. Key stability is
// what makes cache reuse (and therefore the savings) possible.
func TestGeminiCacheKey_Stability(t *testing.T) {
	tools := []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "read_file"}}}}

	base := geminiCacheKey("gemini-2.5-pro", "you are helpful", tools)
	same := geminiCacheKey("gemini-2.5-pro", "you are helpful", tools)
	if base != same {
		t.Errorf("identical inputs produced different keys: %s vs %s", base, same)
	}

	if base == geminiCacheKey("gemini-2.5-flash", "you are helpful", tools) {
		t.Error("different model produced same key")
	}
	if base == geminiCacheKey("gemini-2.5-pro", "you are TERSE", tools) {
		t.Error("different system prompt produced same key")
	}
	otherTools := []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "write_file"}}}}
	if base == geminiCacheKey("gemini-2.5-pro", "you are helpful", otherTools) {
		t.Error("different tool set produced same key")
	}
}

// TestApplyCachedContentToConfig verifies that pointing a config at a cache
// resource clears the now-redundant inline system instruction / tools.
func TestApplyCachedContentToConfig(t *testing.T) {
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "sys"}}},
		Tools:             []*genai.Tool{{}},
		ToolConfig:        &genai.ToolConfig{},
	}
	applyCachedContentToConfig(cfg, "projects/p/locations/l/cachedContents/xyz")

	if cfg.CachedContent != "projects/p/locations/l/cachedContents/xyz" {
		t.Errorf("CachedContent = %q; want the resource name", cfg.CachedContent)
	}
	if cfg.SystemInstruction != nil {
		t.Error("SystemInstruction not cleared after referencing cache resource")
	}
	if cfg.Tools != nil {
		t.Error("Tools not cleared after referencing cache resource")
	}
	if cfg.ToolConfig != nil {
		t.Error("ToolConfig not cleared after referencing cache resource")
	}
}

// TestMaybeApplyGeminiCache_NoopWhenPlanEmpty verifies that when the planner
// does not deem the system prefix worth caching, the config is left untouched
// (no CachedContent, system instruction preserved) — a true no-op fast path.
func TestMaybeApplyGeminiCache_NoopWhenPlanEmpty(t *testing.T) {
	vc := &VertexClient{} // client is nil; function must short-circuit safely
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "sys"}}},
	}
	plan := CachePlan{CacheSystem: false, FirstUserTurnIdx: -1, TailTurnIdx: -1}

	vc.MaybeApplyGeminiCache(context.TODO(), "gemini-2.5-pro", "sys", nil, cfg, plan)

	if cfg.CachedContent != "" {
		t.Errorf("CachedContent = %q; want empty for empty plan", cfg.CachedContent)
	}
	if cfg.SystemInstruction == nil {
		t.Error("SystemInstruction was cleared on a no-op path")
	}
}

// TestMaybeApplyGeminiCache_NoopBelowMinBytes verifies that even with
// CacheSystem true, a system prefix below the Gemini explicit-cache minimum is
// left uncached (avoids a create the API would reject as too small).
func TestMaybeApplyGeminiCache_NoopBelowMinBytes(t *testing.T) {
	vc := &VertexClient{}
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "small"}}},
	}
	plan := CachePlan{CacheSystem: true, FirstUserTurnIdx: -1, TailTurnIdx: -1}

	// systemPrompt well below geminiCacheMinBytes (128k) => no create attempted.
	vc.MaybeApplyGeminiCache(context.TODO(), "gemini-2.5-pro", "small system prompt", nil, cfg, plan)

	if cfg.CachedContent != "" {
		t.Errorf("CachedContent = %q; want empty below min bytes", cfg.CachedContent)
	}
}
