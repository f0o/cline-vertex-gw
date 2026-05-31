package provider

import (
	"go.f0o.dev/cline-vertex-gw/pkg/pipeline"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/genai"
)

func TestIsAnthropicReasoningModel(t *testing.T) {
	cases := map[string]bool{
		// Reasoning / extended-thinking family — sampling params rejected.
		"claude-opus-4":              true,
		"claude-opus-4-7":            true,
		"claude-opus-4-1":            true,
		"claude-opus-4@20251015":     true,
		"claude-sonnet-4":            true,
		"claude-sonnet-4-5":          true,
		"claude-opus-4-thinking":     true,
		"claude-3-7-sonnet-thinking": true,

		// Non-reasoning classics — must keep sampling params.
		"claude-3-5-sonnet":        false,
		"claude-3-5-sonnet-v2":     false,
		"claude-3-5-haiku":         false,
		"claude-3-opus":            false,
		"claude-3-haiku":           false,
		"claude-3-sonnet":          false,
		"claude-3-5-sonnet@latest": false,
		"claude-instant-1.2":       false,

		// Edge cases — defensive.
		"":                 false,
		"gemini-2.0-flash": false,
		"llama-3.3-70b":    false,
	}
	for in, want := range cases {
		if got := isAnthropicReasoningModel(in); got != want {
			t.Errorf("isAnthropicReasoningModel(%q) = %v; want %v", in, got, want)
		}
	}
}

// TestBuildAnthropicRequest_ReasoningModelDropsSampling verifies that the
// Vertex 400 "`temperature` is deprecated for this model." failure observed
// against claude-opus-4-7 doesn't recur: sampling params must be omitted
// from the wire body even when the caller supplies them.
func TestBuildAnthropicRequest_ReasoningModelDropsSampling(t *testing.T) {
	var temp float32 = 0.7
	var topP float32 = 0.9
	var topK int32 = 40
	var maxTok int32 = 256
	opts := &pipeline.GenerationOptions{
		Temperature: &temp,
		TopP:        &topP,
		TopK:        &topK,
		MaxTokens:   &maxTok,
		Stop:        []string{"</done>"},
	}
	contents := []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: "hi"}},
	}}

	req := buildAnthropicRequest("claude-opus-4-7", "be terse", contents, opts, true)

	if req.Temperature != nil {
		t.Errorf("Temperature = %v; want nil for reasoning model", *req.Temperature)
	}
	if req.TopP != nil {
		t.Errorf("TopP = %v; want nil for reasoning model", *req.TopP)
	}
	if req.TopK != nil {
		t.Errorf("TopK = %v; want nil for reasoning model", *req.TopK)
	}
	// Stop sequences and max_tokens MUST still be honored.
	if len(req.StopSequences) != 1 || req.StopSequences[0] != "</done>" {
		t.Errorf("StopSequences = %v; want []string{\"/done\"}", req.StopSequences)
	}
	if req.MaxTokens != 256 {
		t.Errorf("MaxTokens = %d; want 256 (caller override)", req.MaxTokens)
	}
	// Sanity: messages still flowed through.
	if len(req.Messages) != 1 || req.Messages[0].Content != "hi" {
		t.Errorf("Messages = %+v; want one user 'hi' message", req.Messages)
	}
	// "be terse" is below the minCacheableBytes threshold so the System slot
	// stays in plain-text form.
	if req.System == nil || req.System.Text != "be terse" || len(req.System.Blocks) != 0 {
		t.Errorf("System = %+v; want plain-text 'be terse'", req.System)
	}
	if !req.Stream {
		t.Errorf("Stream = false; want true")
	}
}

// TestBuildAnthropicRequest_NonReasoningKeepsSampling verifies that classic
// Claude models (3.5 Sonnet, Haiku, etc.) still receive temperature/top_p/top_k
// so users keep their sampling control.
func TestBuildAnthropicRequest_NonReasoningKeepsSampling(t *testing.T) {
	var temp float32 = 0.3
	var topP float32 = 0.95
	var topK int32 = 50
	opts := &pipeline.GenerationOptions{
		Temperature: &temp,
		TopP:        &topP,
		TopK:        &topK,
	}
	contents := []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: "hi"}},
	}}

	req := buildAnthropicRequest("claude-3-5-sonnet", "", contents, opts, false)

	if req.Temperature == nil || *req.Temperature != 0.3 {
		t.Errorf("Temperature = %v; want pointer to 0.3", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.95 {
		t.Errorf("TopP = %v; want pointer to 0.95", req.TopP)
	}
	if req.TopK == nil || *req.TopK != 50 {
		t.Errorf("TopK = %v; want pointer to 50", req.TopK)
	}
	if req.MaxTokens != defaultAnthropicMaxTokens {
		t.Errorf("MaxTokens = %d; want default %d", req.MaxTokens, defaultAnthropicMaxTokens)
	}
}

// TestBuildAnthropicRequest_NilOptsAndReasoningModel ensures the no-opts code
// path doesn't accidentally allocate sampling fields for reasoning models.
func TestBuildAnthropicRequest_NilOptsAndReasoningModel(t *testing.T) {
	contents := []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: "hi"}},
	}}
	req := buildAnthropicRequest("claude-sonnet-4-5", "", contents, nil, false)
	if req.Temperature != nil || req.TopP != nil || req.TopK != nil {
		t.Errorf("expected all sampling fields nil; got temp=%v topP=%v topK=%v",
			req.Temperature, req.TopP, req.TopK)
	}
	if req.MaxTokens != defaultAnthropicMaxTokens {
		t.Errorf("MaxTokens = %d; want default %d", req.MaxTokens, defaultAnthropicMaxTokens)
	}
}

// bigStr returns a deterministic string of length n. Used to push payloads
// past the minCacheableBytes (4000) threshold without bloating test source.
func bigStr(n int) string {
	const block = "The quick brown fox jumps over the lazy dog. "
	var sb strings.Builder
	for sb.Len() < n {
		sb.WriteString(block)
	}
	return sb.String()[:n]
}

// TestBuildAnthropicRequest_PromptCacheTagsLargeSystem verifies that a system
// prompt above the caching threshold is emitted as a single content block
// carrying `cache_control: {"type":"ephemeral"}` WHEN the conversation is
// multi-turn (so a later request can read the cache back). This is the
// highest-ROI optimization for Cline (huge stable system prompt on every turn).
// Caching is always-on for supported models; no env knob to toggle.
func TestBuildAnthropicRequest_PromptCacheTagsLargeSystem(t *testing.T) {
	bigSys := bigStr(5000) // > 4000-byte threshold
	// Multi-turn: a follow-up turn exists so the cached system prefix WILL be
	// read back on a subsequent request — caching is net-positive here.
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ack"}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "follow-up"}}},
	}
	req := buildAnthropicRequest("claude-3-5-sonnet", bigSys, contents, nil, true)

	if req.System == nil {
		t.Fatal("System = nil; want non-nil with cached block")
	}
	if len(req.System.Blocks) != 1 {
		t.Fatalf("System.Blocks len = %d; want 1", len(req.System.Blocks))
	}
	b := req.System.Blocks[0]
	if b.Type != "text" || b.Text != bigSys {
		t.Errorf("System block = %+v; want type=text + full text", b)
	}
	if b.CacheControl == nil || b.CacheControl.Type != "ephemeral" {
		t.Errorf("System cache_control = %+v; want {ephemeral}", b.CacheControl)
	}
}

// TestBuildAnthropicRequest_PromptCacheSkipsLargeSystemSingleTurn verifies the
// core economic fix: a one-shot call (single user turn, no follow-up) does NOT
// cache even a large system prompt, because there is no later request position
// to read the cache back — the 25% write premium would be pure loss.
func TestBuildAnthropicRequest_PromptCacheSkipsLargeSystemSingleTurn(t *testing.T) {
	bigSys := bigStr(5000) // > 4000-byte threshold, but single-turn
	contents := []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: "hi"}},
	}}
	req := buildAnthropicRequest("claude-3-5-sonnet", bigSys, contents, nil, true)

	if req.System == nil {
		t.Fatal("System = nil; want plain-text system block")
	}
	if len(req.System.Blocks) != 0 {
		t.Errorf("System.Blocks len = %d; want 0 (plain-text, uncached) for single-turn", len(req.System.Blocks))
	}
	if req.System.Text != bigSys {
		t.Errorf("System.Text mismatch; want full plain-text system prompt")
	}
}

// TestBuildAnthropicRequest_PromptCacheTailBreakpoint verifies that a long
// session gets a rolling tail cache breakpoint on the second-to-last turn (so
// the growing history is cached for the next request), in addition to the head
// breakpoints. Total breakpoints must stay within Anthropic's limit of 4.
func TestBuildAnthropicRequest_PromptCacheTailBreakpoint(t *testing.T) {
	bigSys := bigStr(5000)
	var contents []*genai.Content
	for i := 0; i < 8; i++ {
		if i%2 == 0 {
			contents = append(contents, &genai.Content{
				Role: genai.RoleUser, Parts: []*genai.Part{{Text: bigStr(3000)}}})
		} else {
			contents = append(contents, &genai.Content{
				Role: genai.RoleModel, Parts: []*genai.Part{{Text: bigStr(3000)}}})
		}
	}
	req := buildAnthropicRequest("claude-3-5-sonnet", bigSys, contents, nil, true)

	if len(req.Messages) != 8 {
		t.Fatalf("Messages len = %d; want 8", len(req.Messages))
	}
	// The second-to-last turn (index 6) should carry a cache_control marker.
	tail := req.Messages[6]
	if len(tail.Blocks) == 0 || tail.Blocks[len(tail.Blocks)-1].CacheControl == nil {
		t.Errorf("Messages[6] (tail anchor) = %+v; want a cache_control marker", tail)
	}
	// Count total cache_control markers across system + messages; must be ≤ 4.
	markers := 0
	if req.System != nil {
		for _, b := range req.System.Blocks {
			if b.CacheControl != nil {
				markers++
			}
		}
	}
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.CacheControl != nil {
				markers++
			}
		}
	}
	if markers > 4 {
		t.Errorf("total cache_control markers = %d; want ≤ 4", markers)
	}
}

// TestBuildAnthropicRequest_PromptCacheTagsFirstUserTurn verifies that with
// a multi-turn conversation, the FIRST user turn gets a cache_control marker
// but later user turns don't. (Caching is always-on for supported models.)
func TestBuildAnthropicRequest_PromptCacheTagsFirstUserTurn(t *testing.T) {
	bigUser := bigStr(5000)
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: bigUser}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ack"}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "follow-up question"}}},
	}
	req := buildAnthropicRequest("claude-3-5-sonnet", "", contents, nil, true)

	if len(req.Messages) != 3 {
		t.Fatalf("Messages len = %d; want 3", len(req.Messages))
	}

	// Index 0: first user turn, should be a cached block.
	m0 := req.Messages[0]
	if m0.Role != "user" || len(m0.Blocks) != 1 {
		t.Fatalf("Messages[0] = %+v; want user with one block", m0)
	}
	if m0.Blocks[0].CacheControl == nil || m0.Blocks[0].CacheControl.Type != "ephemeral" {
		t.Errorf("Messages[0] cache_control = %+v; want {ephemeral}", m0.Blocks[0].CacheControl)
	}
	if m0.Blocks[0].Text != bigUser {
		t.Errorf("Messages[0] text mismatch")
	}

	// Index 1: assistant — plain string, no caching.
	if req.Messages[1].Role != "assistant" || len(req.Messages[1].Blocks) != 0 || req.Messages[1].Content != "ack" {
		t.Errorf("Messages[1] = %+v; want plain assistant 'ack'", req.Messages[1])
	}

	// Index 2: subsequent user turn — plain string, no caching.
	if req.Messages[2].Role != "user" || len(req.Messages[2].Blocks) != 0 || req.Messages[2].Content != "follow-up question" {
		t.Errorf("Messages[2] = %+v; want plain user 'follow-up question'", req.Messages[2])
	}
}

// TestBuildAnthropicRequest_PromptCacheSkipsShortPayload ensures we don't
// pay the structural cost of cache_control tagging when the payload is too
// small to benefit (Anthropic ignores markers below the minimum prefix size
// anyway, but the cleaner wire is easier to debug + matches the docs).
func TestBuildAnthropicRequest_PromptCacheSkipsShortPayload(t *testing.T) {
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "tiny user"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "tiny ack"}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "tiny follow"}}},
	}
	req := buildAnthropicRequest("claude-3-5-sonnet", "short sys", contents, nil, true)

	if req.System == nil || req.System.Text != "short sys" || len(req.System.Blocks) != 0 {
		t.Errorf("System = %+v; want plain-text for sub-threshold payload", req.System)
	}
	for i, m := range req.Messages {
		if len(m.Blocks) > 0 {
			t.Errorf("Messages[%d] uses Blocks; want plain string for sub-threshold payload", i)
		}
	}
}

// TestBuildAnthropicRequest_PromptCacheSkipsSingleTurn verifies that a
// one-shot call (single user turn, no follow-ups) does NOT cache the user
// content, since there's no later request to hit the cache.
func TestBuildAnthropicRequest_PromptCacheSkipsSingleTurn(t *testing.T) {
	bigUser := bigStr(5000)
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: bigUser}}},
	}
	req := buildAnthropicRequest("claude-3-5-sonnet", "", contents, nil, true)

	if len(req.Messages) != 1 {
		t.Fatalf("Messages len = %d; want 1", len(req.Messages))
	}
	if len(req.Messages[0].Blocks) != 0 {
		t.Errorf("Messages[0] uses Blocks; want plain-string for single-turn call")
	}
	if req.Messages[0].Content != bigUser {
		t.Errorf("Messages[0].Content text mismatch")
	}
}

// TestAnthropicMessage_MarshalJSON_Polymorphic ensures the custom marshaller
// emits the two valid Anthropic wire shapes (bare string vs block array)
// depending on whether Blocks is populated. Both shapes are accepted by the
// upstream; mixing them up would break either caching or vanilla calls.
func TestAnthropicMessage_MarshalJSON_Polymorphic(t *testing.T) {
	t.Run("plain string", func(t *testing.T) {
		m := anthropicMessage{Role: "user", Content: "hello"}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `{"role":"user","content":"hello"}`
		if string(raw) != want {
			t.Errorf("got %s; want %s", raw, want)
		}
	})

	t.Run("block array", func(t *testing.T) {
		m := anthropicMessage{
			Role: "user",
			Blocks: []anthropicTextBlock{{
				Type:         "text",
				Text:         "hello",
				CacheControl: &anthropicCacheControl{Type: "ephemeral"},
			}},
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}`
		if string(raw) != want {
			t.Errorf("got %s; want %s", raw, want)
		}
	})
}

// TestAnthropicSystem_MarshalJSON_Polymorphic mirrors the message-shape test
// but for the system slot. Anthropic accepts either shape on the wire.
func TestAnthropicSystem_MarshalJSON_Polymorphic(t *testing.T) {
	t.Run("plain string", func(t *testing.T) {
		s := anthropicSystem{Text: "be terse"}
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(raw) != `"be terse"` {
			t.Errorf("got %s; want 'be terse'", raw)
		}
	})

	t.Run("block array", func(t *testing.T) {
		s := anthropicSystem{
			Blocks: []anthropicTextBlock{{
				Type:         "text",
				Text:         "be terse",
				CacheControl: &anthropicCacheControl{Type: "ephemeral"},
			}},
		}
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `[{"type":"text","text":"be terse","cache_control":{"type":"ephemeral"}}]`
		if string(raw) != want {
			t.Errorf("got %s; want %s", raw, want)
		}
	})
}
