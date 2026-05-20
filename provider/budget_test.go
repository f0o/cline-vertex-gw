package provider

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// withMaxInputChars temporarily overrides the package-level budget for tests
// and returns a restore func.
func withMaxInputChars(t *testing.T, v int32) func() {
	t.Helper()
	prev := maxInputChars
	maxInputChars = v
	return func() { maxInputChars = prev }
}

// mkTurn builds a one-part Content with the given role and text. Keeps the
// table-driven tests below compact.
func mkTurn(role, text string) *genai.Content {
	return &genai.Content{Role: role, Parts: []*genai.Part{{Text: text}}}
}

// TestTrimContents_Disabled verifies the zero-knob fast path: with the
// budget unset the function MUST return the input slice unchanged.
func TestTrimContents_Disabled(t *testing.T) {
	defer withMaxInputChars(t, 0)()

	in := []*genai.Content{
		mkTurn(genai.RoleUser, strings.Repeat("a", 10_000)),
		mkTurn(genai.RoleModel, strings.Repeat("b", 10_000)),
		mkTurn(genai.RoleUser, "tiny"),
	}
	out := TrimContents(in, "huge system prompt")
	if len(out) != len(in) {
		t.Errorf("len(out) = %d; want %d (no trim when budget=0)", len(out), len(in))
	}
}

// TestTrimContents_UnderBudget verifies that a conversation already fitting
// the budget is returned unchanged (same slice header).
func TestTrimContents_UnderBudget(t *testing.T) {
	defer withMaxInputChars(t, 100_000)()

	in := []*genai.Content{
		mkTurn(genai.RoleUser, "hello"),
		mkTurn(genai.RoleModel, "world"),
	}
	out := TrimContents(in, "sys")
	if len(out) != 2 {
		t.Errorf("len(out) = %d; want 2 (already fits)", len(out))
	}
}

// TestTrimContents_DropsOldest verifies that when over budget, the oldest
// turns are dropped first while the most recent ones (including the final
// user message) are preserved.
func TestTrimContents_DropsOldest(t *testing.T) {
	defer withMaxInputChars(t, 1000)()

	in := []*genai.Content{
		mkTurn(genai.RoleUser, strings.Repeat("a", 400)),    // 0 - should drop
		mkTurn(genai.RoleModel, strings.Repeat("b", 400)),   // 1 - should drop
		mkTurn(genai.RoleUser, strings.Repeat("c", 400)),    // 2 - keep
		mkTurn(genai.RoleModel, strings.Repeat("d", 400)),   // 3 - keep
		mkTurn(genai.RoleUser, "final question"),            // 4 - keep
	}
	out := TrimContents(in, "")
	if len(out) >= len(in) {
		t.Fatalf("len(out) = %d; want < %d (some turns should be dropped)", len(out), len(in))
	}
	// The final turn (the actual question) MUST always survive.
	last := out[len(out)-1]
	if last.Parts[0].Text != "final question" {
		t.Errorf("last kept turn = %q; want final user message preserved", last.Parts[0].Text)
	}
	// Total kept size must fit budget.
	totalBytes := 0
	for _, c := range out {
		totalBytes += contentBytes(c)
	}
	if totalBytes > 1000 {
		t.Errorf("kept bytes = %d; want <= 1000 (budget)", totalBytes)
	}
}

// TestTrimContents_KeepsLastTurnEvenIfHuge verifies the minRetainedTurns
// floor: if even a single message exceeds the budget, the function must
// still keep at least one turn rather than returning an empty slice (which
// would cause the upstream call to fail).
func TestTrimContents_KeepsLastTurnEvenIfHuge(t *testing.T) {
	defer withMaxInputChars(t, 100)()

	in := []*genai.Content{
		mkTurn(genai.RoleUser, strings.Repeat("a", 5000)),
		mkTurn(genai.RoleModel, strings.Repeat("b", 5000)),
		mkTurn(genai.RoleUser, strings.Repeat("c", 5000)), // huge final turn
	}
	out := TrimContents(in, "")
	if len(out) < 1 {
		t.Fatalf("len(out) = %d; want >= 1 (minRetainedTurns floor)", len(out))
	}
	last := out[len(out)-1]
	if !strings.HasPrefix(last.Parts[0].Text, "c") {
		t.Errorf("last kept turn doesn't start with 'c'; lost the final user message")
	}
}

// TestTrimContents_SystemPromptCountsTowardBudget verifies the system prompt
// is subtracted from the budget — if it's huge, fewer history turns fit.
func TestTrimContents_SystemPromptCountsTowardBudget(t *testing.T) {
	defer withMaxInputChars(t, 1000)()

	bigSys := strings.Repeat("s", 800) // eats 800/1000 of budget
	in := []*genai.Content{
		mkTurn(genai.RoleUser, strings.Repeat("a", 300)),  // 0 - won't fit
		mkTurn(genai.RoleModel, strings.Repeat("b", 100)), // 1 - might
		mkTurn(genai.RoleUser, strings.Repeat("c", 50)),   // 2 - will
	}
	out := TrimContents(in, bigSys)
	if len(out) >= len(in) {
		t.Errorf("len(out) = %d; want < %d (large sys prompt should force trim)", len(out), len(in))
	}
	// Final turn always preserved.
	if out[len(out)-1].Parts[0].Text != strings.Repeat("c", 50) {
		t.Errorf("final user turn not preserved after sys-prompt-driven trim")
	}
}

// TestTrimContents_SystemPromptOverBudget verifies the degenerate case where
// the system prompt alone exceeds the budget: we keep only the floor turns.
func TestTrimContents_SystemPromptOverBudget(t *testing.T) {
	defer withMaxInputChars(t, 100)()

	in := []*genai.Content{
		mkTurn(genai.RoleUser, "a"),
		mkTurn(genai.RoleModel, "b"),
		mkTurn(genai.RoleUser, "c"),
	}
	out := TrimContents(in, strings.Repeat("s", 500))
	if len(out) != minRetainedTurns {
		t.Errorf("len(out) = %d; want %d (only floor kept when sys exceeds budget)",
			len(out), minRetainedTurns)
	}
	if out[len(out)-1].Parts[0].Text != "c" {
		t.Errorf("kept turn = %+v; want last user message 'c'", out[0])
	}
}

// TestTrimContents_DoesNotMutateInput verifies that the input slice's header
// (length / underlying array layout) is unchanged after trimming — callers
// may share the slice across goroutines or retries.
func TestTrimContents_DoesNotMutateInput(t *testing.T) {
	defer withMaxInputChars(t, 500)()

	in := []*genai.Content{
		mkTurn(genai.RoleUser, strings.Repeat("a", 400)),
		mkTurn(genai.RoleModel, strings.Repeat("b", 400)),
		mkTurn(genai.RoleUser, "tail"),
	}
	originalLen := len(in)
	_ = TrimContents(in, "")
	if len(in) != originalLen {
		t.Errorf("input slice length mutated: was %d, now %d", originalLen, len(in))
	}
	// First element must still be reachable (we didn't nil it out).
	if in[0] == nil || in[0].Parts[0].Text != strings.Repeat("a", 400) {
		t.Errorf("input slice contents mutated")
	}
}