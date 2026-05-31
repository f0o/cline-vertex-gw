package provider

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// userTurn / modelTurn build a genai.Content of n bytes of text for the given
// role, so tests can drive the planner with controlled turn sizes.
func userTurn(n int) *genai.Content {
	return &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: bigStr(n)}}}
}

func modelTurn(n int) *genai.Content {
	return &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: bigStr(n)}}}
}

func TestCacheCapabilityFor(t *testing.T) {
	cases := map[string]CacheCapability{
		"anthropic":   CacheExplicitInline,
		"google":      CacheExplicitResource,
		"cohere":      CacheNone,
		"meta":        CacheImplicit,
		"mistralai":   CacheImplicit,
		"deepseek-ai": CacheImplicit,
		"qwen":        CacheImplicit,
		"ai21":        CacheImplicit,
		"nvidia":      CacheImplicit,
		"unknown-x":   CacheNone,
	}
	for pub, want := range cases {
		if got := CacheCapabilityFor(pub); got != want {
			t.Errorf("CacheCapabilityFor(%q) = %v; want %v", pub, got, want)
		}
	}
}

// TestPlanCache_SingleTurnNoCache: a one-shot call (single user turn, no
// follow-up) must NOT cache anything, even with a huge system prompt — there
// is no later request position to read the cache back, so the write premium
// would be pure loss. This is the core economic fix.
func TestPlanCache_SingleTurnNoCache(t *testing.T) {
	bigSys := bigStr(8000)
	contents := []*genai.Content{userTurn(8000)}
	plan := PlanCache(contents, bigSys, "anthropic")
	if !plan.IsEmpty() {
		t.Errorf("single-turn plan = %+v; want empty (no read-back position)", plan)
	}
}

// TestPlanCache_MultiTurnHead: a multi-turn conversation with a large system
// prompt and large first user turn should cache BOTH the system prefix and the
// first-user prefix (the stable session head).
func TestPlanCache_MultiTurnHead(t *testing.T) {
	bigSys := bigStr(8000)
	contents := []*genai.Content{
		userTurn(8000), // 0: first user (large)
		modelTurn(200), // 1: assistant
		userTurn(200),  // 2: follow-up (current question)
	}
	plan := PlanCache(contents, bigSys, "anthropic")
	if !plan.CacheSystem {
		t.Errorf("CacheSystem = false; want true for large system + multi-turn")
	}
	if plan.FirstUserTurnIdx != 0 {
		t.Errorf("FirstUserTurnIdx = %d; want 0", plan.FirstUserTurnIdx)
	}
}

// TestPlanCache_SmallSystemNotCached: a sub-threshold system prompt must not be
// cached even in a multi-turn conversation.
func TestPlanCache_SmallSystemNotCached(t *testing.T) {
	contents := []*genai.Content{
		userTurn(8000),
		modelTurn(200),
		userTurn(200),
	}
	plan := PlanCache(contents, "short system", "anthropic")
	if plan.CacheSystem {
		t.Errorf("CacheSystem = true; want false for sub-threshold system prompt")
	}
	// First user turn is large + has follow-ups => still cached.
	if plan.FirstUserTurnIdx != 0 {
		t.Errorf("FirstUserTurnIdx = %d; want 0", plan.FirstUserTurnIdx)
	}
}

// TestPlanCache_SmallFirstUserNotCached: a small first user turn must not get a
// breakpoint (the prefix is too small to clear the minimum cacheable size).
func TestPlanCache_SmallFirstUserNotCached(t *testing.T) {
	contents := []*genai.Content{
		userTurn(200), // small first user
		modelTurn(200),
		userTurn(200),
	}
	plan := PlanCache(contents, "short", "anthropic")
	if plan.FirstUserTurnIdx != -1 {
		t.Errorf("FirstUserTurnIdx = %d; want -1 for sub-threshold first user", plan.FirstUserTurnIdx)
	}
}

// TestPlanCache_LongSessionTailBreakpoint: a long session (>= tail-min-turns
// and >= tail-min-bytes prefix) should get a rolling tail breakpoint on the
// second-to-last turn so the growing history is cached for the next request.
func TestPlanCache_LongSessionTailBreakpoint(t *testing.T) {
	// 8 turns, each 3000 bytes => prefix to anchor (idx 6) ≈ 21000 bytes,
	// above the 16000 tail-min-bytes default; 8 turns >= 6 tail-min-turns.
	var contents []*genai.Content
	for i := 0; i < 8; i++ {
		if i%2 == 0 {
			contents = append(contents, userTurn(3000))
		} else {
			contents = append(contents, modelTurn(3000))
		}
	}
	plan := PlanCache(contents, bigStr(8000), "anthropic")
	if plan.TailTurnIdx != len(mergedTurns(contents))-2 {
		t.Errorf("TailTurnIdx = %d; want second-to-last (%d)",
			plan.TailTurnIdx, len(mergedTurns(contents))-2)
	}
	if plan.BreakpointCount() > maxCacheBreakpoints {
		t.Errorf("BreakpointCount = %d; exceeds cap %d", plan.BreakpointCount(), maxCacheBreakpoints)
	}
}

// TestPlanCache_ShortSessionNoTail: a short multi-turn session must NOT get a
// tail breakpoint (too few turns to confidently re-read it).
func TestPlanCache_ShortSessionNoTail(t *testing.T) {
	contents := []*genai.Content{
		userTurn(8000),
		modelTurn(200),
		userTurn(200),
	}
	plan := PlanCache(contents, bigStr(8000), "anthropic")
	if plan.TailTurnIdx != -1 {
		t.Errorf("TailTurnIdx = %d; want -1 for short session", plan.TailTurnIdx)
	}
}

// TestPlanCache_ImplicitProviderEmpty: implicit-cache providers (OpenAI-compat)
// and no-cache providers (cohere) get an empty plan — we cannot place
// breakpoints, so the planner proposes nothing.
func TestPlanCache_ImplicitAndNoneProviderEmpty(t *testing.T) {
	contents := []*genai.Content{
		userTurn(8000),
		modelTurn(3000),
		userTurn(200),
	}
	for _, pub := range []string{"meta", "deepseek-ai", "qwen", "cohere"} {
		plan := PlanCache(contents, bigStr(8000), pub)
		if !plan.IsEmpty() {
			t.Errorf("PlanCache(%q) = %+v; want empty (no explicit primitive)", pub, plan)
		}
	}
}

// TestPlanCache_GoogleGetsHeadPlan: the google (Gemini) path uses an explicit
// resource primitive, so it should receive a non-empty plan for a large
// multi-turn conversation (drives CachedContent creation downstream).
func TestPlanCache_GoogleGetsHeadPlan(t *testing.T) {
	contents := []*genai.Content{
		userTurn(8000),
		modelTurn(200),
		userTurn(200),
	}
	plan := PlanCache(contents, bigStr(8000), "google")
	if !plan.CacheSystem {
		t.Errorf("google CacheSystem = false; want true for large system + multi-turn")
	}
}

// TestPlanCache_MasterSwitchOff verifies GW_PROMPT_CACHE=off yields empty plans.
func TestPlanCache_MasterSwitchOff(t *testing.T) {
	orig := promptCacheEnabled
	promptCacheEnabled = false
	defer func() { promptCacheEnabled = orig }()

	contents := []*genai.Content{
		userTurn(8000),
		modelTurn(200),
		userTurn(200),
	}
	plan := PlanCache(contents, bigStr(8000), "anthropic")
	if !plan.IsEmpty() {
		t.Errorf("plan with master switch off = %+v; want empty", plan)
	}
}

// TestMergedTurns_CollapsesSameRole verifies consecutive same-role contents are
// merged into a single turn (matching adapter behavior) and sizes accumulate.
func TestMergedTurns_CollapsesSameRole(t *testing.T) {
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: strings.Repeat("a", 100)}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: strings.Repeat("b", 50)}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}},
	}
	turns := mergedTurns(contents)
	if len(turns) != 2 {
		t.Fatalf("merged turns = %d; want 2", len(turns))
	}
	if turns[0].role != "user" || turns[0].bytes != 150 {
		t.Errorf("turn0 = %+v; want {user 150}", turns[0])
	}
	if turns[1].role != "assistant" || turns[1].bytes != 2 {
		t.Errorf("turn1 = %+v; want {assistant 2}", turns[1])
	}
}
