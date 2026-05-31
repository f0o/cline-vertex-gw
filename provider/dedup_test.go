package provider

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// withDedup temporarily overrides the dedup knobs for tests.
func withDedup(t *testing.T, enabled bool, minBytes int32) func() {
	t.Helper()
	prevEnabled := dedupReplay
	prevMin := dedupMinBytes
	dedupReplay = enabled
	dedupMinBytes = minBytes
	return func() {
		dedupReplay = prevEnabled
		dedupMinBytes = prevMin
	}
}

// TestDedup_Disabled verifies fast-path when the knob is off.
func TestDedup_Disabled(t *testing.T) {
	defer withDedup(t, false, 100)()
	big := strings.Repeat("A", 2000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, big),
	}
	out := DedupReplayedBlocks(in)
	if &out[0] != &in[0] {
		t.Error("expected fast-path return when disabled")
	}
}

// TestDedup_BasicRepeat verifies that a verbatim repeat of a large user
// block is replaced with a placeholder pointing at the first occurrence.
func TestDedup_BasicRepeat(t *testing.T) {
	defer withDedup(t, true, 100)()
	big := strings.Repeat("A", 2000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big),              // turn 1 - kept verbatim
		mkTurn(genai.RoleModel, "ack"),           // turn 2
		mkTurn(genai.RoleUser, "now this: "+big), // turn 3 - DIFFERENT (prefix added)
		mkTurn(genai.RoleUser, big),              // turn 4 - DUPLICATE of turn 1
	}
	out := DedupReplayedBlocks(in)
	if out[0].Parts[0].Text != big {
		t.Errorf("first occurrence was modified")
	}
	if out[2].Parts[0].Text != "now this: "+big {
		t.Errorf("differing-prefix turn was incorrectly modified")
	}
	// Turn 4 should be a placeholder pointing at turn 1.
	got := out[3].Parts[0].Text
	if !strings.Contains(got, "identical content already shown in turn 1") {
		t.Errorf("dup turn 4 not pointing at turn 1: %q", got)
	}
	if strings.Contains(got, big) {
		t.Errorf("dup turn 4 still contains the original body")
	}
}

// TestDedup_BelowThreshold verifies blocks below GW_DEDUP_MIN_BYTES are
// passed through verbatim even if they're exact duplicates.
func TestDedup_BelowThreshold(t *testing.T) {
	defer withDedup(t, true, 10_000)()
	small := strings.Repeat("S", 500)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, small),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, small),
	}
	out := DedupReplayedBlocks(in)
	if out[2].Parts[0].Text != small {
		t.Errorf("below-threshold dup was incorrectly replaced: %q", out[2].Parts[0].Text)
	}
}

// TestDedup_RoleScoped verifies a user-turn block doesn't dedupe against
// an assistant-turn block of the same text — speaker matters.
func TestDedup_RoleScoped(t *testing.T) {
	defer withDedup(t, true, 100)()
	big := strings.Repeat("R", 2000)
	in := []*genai.Content{
		mkTurn(genai.RoleModel, big), // assistant turn says it first
		mkTurn(genai.RoleUser, big),  // user turn with same bytes — must NOT dedup
	}
	out := DedupReplayedBlocks(in)
	if out[1].Parts[0].Text != big {
		t.Errorf("user turn incorrectly deduped against assistant turn: %q",
			out[1].Parts[0].Text)
	}
}

// TestDedup_PlaceholderShape verifies the placeholder text contains the
// byte count, turn reference, and a hash hint for grepability.
func TestDedup_PlaceholderShape(t *testing.T) {
	defer withDedup(t, true, 100)()
	big := strings.Repeat("Z", 1024)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, big),
	}
	out := DedupReplayedBlocks(in)
	got := out[2].Parts[0].Text
	if !strings.Contains(got, "1024 bytes elided") {
		t.Errorf("placeholder missing byte-count: %q", got)
	}
	if !strings.Contains(got, "sha256=") {
		t.Errorf("placeholder missing hash hint: %q", got)
	}
	if !strings.Contains(got, "turn 1") {
		t.Errorf("placeholder missing turn reference: %q", got)
	}
}

// TestDedup_DoesNotMutateInput verifies the input slice and its underlying
// Content objects are untouched after the call.
func TestDedup_DoesNotMutateInput(t *testing.T) {
	defer withDedup(t, true, 100)()
	big := strings.Repeat("Q", 2000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, big),
	}
	_ = DedupReplayedBlocks(in)
	if in[2].Parts[0].Text != big {
		t.Errorf("input mutated; turn 3 should still be the original body")
	}
}

// TestDedup_MultipleDuplicates verifies a single block repeated 3+ times
// gets replaced 2+ times (every occurrence beyond the first).
func TestDedup_MultipleDuplicates(t *testing.T) {
	defer withDedup(t, true, 100)()
	big := strings.Repeat("M", 2000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big),    // 1 verbatim
		mkTurn(genai.RoleModel, "ack"), // 2
		mkTurn(genai.RoleUser, big),    // 3 dedup
		mkTurn(genai.RoleModel, "ack"), // 4
		mkTurn(genai.RoleUser, big),    // 5 dedup
	}
	out := DedupReplayedBlocks(in)
	if out[0].Parts[0].Text != big {
		t.Errorf("first occurrence modified")
	}
	for _, i := range []int{2, 4} {
		got := out[i].Parts[0].Text
		if strings.Contains(got, big) {
			t.Errorf("turn %d not deduped: %q", i+1, got)
		}
		if !strings.Contains(got, "turn 1") {
			t.Errorf("turn %d not pointing at turn 1: %q", i+1, got)
		}
	}
}

// TestDedup_MixedParts verifies a Content with multiple parts dedupes each
// part independently.
func TestDedup_MixedParts(t *testing.T) {
	defer withDedup(t, true, 100)()
	a := strings.Repeat("a", 1000)
	b := strings.Repeat("b", 1000)
	in := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: a}, {Text: b}}},
		mkTurn(genai.RoleModel, "ack"),
		// Turn 3 repeats both `a` and `b` — both should dedup.
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: a}, {Text: b}}},
	}
	out := DedupReplayedBlocks(in)
	for j, p := range out[2].Parts {
		if strings.Contains(p.Text, "aaaa") || strings.Contains(p.Text, "bbbb") {
			t.Errorf("part %d not deduped: %q", j, p.Text)
		}
	}
}

// TestDedup_LastTurnNotExempt verifies a duplicate appearing in the final
// turn IS still replaced (the placeholder always points backward, so this
// is safe).
func TestDedup_LastTurnNotExempt(t *testing.T) {
	defer withDedup(t, true, 100)()
	big := strings.Repeat("L", 2000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big), // turn 1
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, big), // turn 3 (LAST)
	}
	out := DedupReplayedBlocks(in)
	if strings.Contains(out[2].Parts[0].Text, big) {
		t.Errorf("last turn's dup was not collapsed: %q", out[2].Parts[0].Text)
	}
}

// TestDedup_NilSafe verifies the function tolerates nil entries and nil
// parts.
func TestDedup_NilSafe(t *testing.T) {
	defer withDedup(t, true, 100)()
	in := []*genai.Content{
		nil,
		{Role: genai.RoleUser, Parts: []*genai.Part{nil, {Text: strings.Repeat("x", 2000)}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{nil, {Text: strings.Repeat("x", 2000)}}},
	}
	out := DedupReplayedBlocks(in)
	if len(out) != 3 {
		t.Fatalf("len(out)=%d; want 3", len(out))
	}
	if out[0] != nil {
		t.Errorf("nil entry not preserved")
	}
	// Second occurrence should be deduped.
	if !strings.Contains(out[2].Parts[1].Text, "turn 2") {
		t.Errorf("second occurrence not deduped: %q", out[2].Parts[1].Text)
	}
}

// TestDedupKey_Stable verifies that dedupKey is deterministic and
// role-scoped.
func TestDedupKey_Stable(t *testing.T) {
	a1 := dedupKey("user", "hello world")
	a2 := dedupKey("user", "hello world")
	b := dedupKey("model", "hello world")
	if a1 != a2 {
		t.Errorf("hash not stable: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("role-scoped keys collided: %q == %q", a1, b)
	}
	// Format changed in 0.9.0 to "<role>|text|<hash>" so we can disambiguate
	// from "<role>|image|<hash>" when image dedup landed. Updated assertion.
	if !strings.HasPrefix(a1, "user|text|") {
		t.Errorf("key missing role+kind prefix: %q", a1)
	}
}
