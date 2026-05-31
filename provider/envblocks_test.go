package provider

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// withCollapseEnv temporarily overrides the env-collapse switch & threshold
// for tests, returning a restore func.
func withCollapseEnv(t *testing.T, enabled bool, minBytes int32) func() {
	t.Helper()
	prevEnabled := collapseEnvBlocks
	prevMin := collapseEnvMinBytes
	collapseEnvBlocks = enabled
	collapseEnvMinBytes = minBytes
	return func() {
		collapseEnvBlocks = prevEnabled
		collapseEnvMinBytes = prevMin
	}
}

// makeEnvBlock builds an env-details block of approximately the given
// total length (open + body + close). Used to drive threshold tests.
func makeEnvBlock(bodyLen int) string {
	body := strings.Repeat("x", bodyLen)
	return envOpenTag + body + envCloseTag
}

// TestCollapseEnvBlocks_Disabled verifies the fast-path returns the input
// slice header unchanged when the knob is off.
func TestCollapseEnvBlocks_Disabled(t *testing.T) {
	defer withCollapseEnv(t, false, 256)()
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "before "+makeEnvBlock(2000)+" after"),
		mkTurn(genai.RoleModel, "ok"),
		mkTurn(genai.RoleUser, "final"),
	}
	out := CollapseEnvBlocks(in)
	if &out[0] != &in[0] {
		t.Error("expected fast-path return when disabled")
	}
}

// TestCollapseEnvBlocks_LastUserExempt verifies the most recent user turn's
// env block is left intact even when oversized.
func TestCollapseEnvBlocks_LastUserExempt(t *testing.T) {
	defer withCollapseEnv(t, true, 100)()
	bigBlock := makeEnvBlock(5000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "old turn "+bigBlock),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, "current turn "+bigBlock),
	}
	out := CollapseEnvBlocks(in)
	// Old user turn must be collapsed.
	if strings.Contains(out[0].Parts[0].Text, strings.Repeat("x", 5000)) {
		t.Error("old user env block was not collapsed")
	}
	// Latest user turn must be untouched.
	if !strings.Contains(out[2].Parts[0].Text, strings.Repeat("x", 5000)) {
		t.Error("last user env block was incorrectly collapsed")
	}
}

// TestCollapseEnvBlocks_AssistantUntouched verifies assistant turns are
// never modified, even if (somehow) they contain env_details markers.
func TestCollapseEnvBlocks_AssistantUntouched(t *testing.T) {
	defer withCollapseEnv(t, true, 100)()
	assistantText := "explaining... " + makeEnvBlock(3000) + " ...end"
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "first"),
		mkTurn(genai.RoleModel, assistantText),
		mkTurn(genai.RoleUser, "second"),
	}
	out := CollapseEnvBlocks(in)
	if out[1].Parts[0].Text != assistantText {
		t.Error("assistant turn was modified")
	}
}

// TestCollapseEnvBlocks_BelowThreshold verifies blocks under the min-bytes
// threshold are preserved verbatim (placeholder overhead would cost more
// than the block itself).
func TestCollapseEnvBlocks_BelowThreshold(t *testing.T) {
	defer withCollapseEnv(t, true, 10_000)()
	small := makeEnvBlock(500) // well under 10k
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "msg "+small),
		mkTurn(genai.RoleModel, "ok"),
		mkTurn(genai.RoleUser, "current"),
	}
	out := CollapseEnvBlocks(in)
	if !strings.Contains(out[0].Parts[0].Text, small) {
		t.Error("below-threshold block was incorrectly collapsed")
	}
}

// TestCollapseEnvBlocks_PlaceholderShape verifies the placeholder text is
// what we promise: env_details tags wrapping a byte-count notice.
func TestCollapseEnvBlocks_PlaceholderShape(t *testing.T) {
	defer withCollapseEnv(t, true, 100)()
	big := makeEnvBlock(2000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "msg "+big),
		mkTurn(genai.RoleModel, "ok"),
		mkTurn(genai.RoleUser, "current"),
	}
	out := CollapseEnvBlocks(in)
	txt := out[0].Parts[0].Text
	if !strings.Contains(txt, envOpenTag) || !strings.Contains(txt, envCloseTag) {
		t.Errorf("placeholder missing env_details tags: %q", txt)
	}
	if !strings.Contains(txt, "stale IDE snapshot") {
		t.Errorf("placeholder missing identifying text: %q", txt)
	}
	// Original byte count of the elided block should be mentioned, so
	// operators can grep the request body and see what's been removed.
	expectedLen := len(big)
	if !strings.Contains(txt, "bytes elided") {
		t.Errorf("placeholder doesn't mention 'bytes elided': %q", txt)
	}
	// Surrounding context preserved.
	if !strings.HasPrefix(txt, "msg ") {
		t.Errorf("context before env block lost: %q", txt)
	}
	// Make sure we actually saved bytes.
	if len(txt) >= len(in[0].Parts[0].Text) {
		t.Errorf("no bytes saved: was %d now %d (orig block %d)",
			len(in[0].Parts[0].Text), len(txt), expectedLen)
	}
}

// TestCollapseEnvBlocks_UnterminatedTagSafe verifies a malformed open tag
// without a close doesn't cause infinite loop or panic.
func TestCollapseEnvBlocks_UnterminatedTagSafe(t *testing.T) {
	defer withCollapseEnv(t, true, 100)()
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "open without close "+envOpenTag+" the rest"),
		mkTurn(genai.RoleModel, "ok"),
		mkTurn(genai.RoleUser, "current"),
	}
	out := CollapseEnvBlocks(in) // must not panic / hang
	if !strings.Contains(out[0].Parts[0].Text, "the rest") {
		t.Error("trailing text after unterminated tag was lost")
	}
}

// TestCollapseEnvBlocks_MultipleBlocksOneTurn verifies the parser handles
// multiple env blocks within a single turn (defensive — not currently
// emitted by Cline but should still work).
func TestCollapseEnvBlocks_MultipleBlocksOneTurn(t *testing.T) {
	defer withCollapseEnv(t, true, 100)()
	doubled := makeEnvBlock(2000) + " middle " + makeEnvBlock(2000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "intro "+doubled+" tail"),
		mkTurn(genai.RoleModel, "ok"),
		mkTurn(genai.RoleUser, "current"),
	}
	out := CollapseEnvBlocks(in)
	txt := out[0].Parts[0].Text
	// Neither original body should still be present.
	if strings.Contains(txt, strings.Repeat("x", 2000)) {
		t.Error("at least one of the two env blocks was not collapsed")
	}
	// Bridging "middle" text must survive.
	if !strings.Contains(txt, "middle") {
		t.Errorf("inter-block text 'middle' was lost: %q", txt)
	}
	// Surrounding "intro" / "tail" must survive.
	if !strings.Contains(txt, "intro") || !strings.Contains(txt, "tail") {
		t.Errorf("surrounding text lost: %q", txt)
	}
}

// TestCollapseEnvBlocks_NoBlocksFastPath verifies that turns containing
// nothing relevant pass through with no allocation pressure beyond the
// shallow-slice copy.
func TestCollapseEnvBlocks_NoBlocksFastPath(t *testing.T) {
	defer withCollapseEnv(t, true, 100)()
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "no env markers here at all"),
		mkTurn(genai.RoleModel, "ok"),
		mkTurn(genai.RoleUser, "still none"),
	}
	out := CollapseEnvBlocks(in)
	if out[0].Parts[0].Text != in[0].Parts[0].Text {
		t.Errorf("text mutated when no blocks present: %q != %q",
			out[0].Parts[0].Text, in[0].Parts[0].Text)
	}
}

// TestCollapseEnvBlocks_DoesNotMutateInput verifies the input slice and
// its underlying Content objects are untouched after the call.
func TestCollapseEnvBlocks_DoesNotMutateInput(t *testing.T) {
	defer withCollapseEnv(t, true, 100)()
	original := "msg " + makeEnvBlock(2000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, original),
		mkTurn(genai.RoleModel, "ok"),
		mkTurn(genai.RoleUser, "current"),
	}
	_ = CollapseEnvBlocks(in)
	if in[0].Parts[0].Text != original {
		t.Errorf("input mutated; was len=%d now len=%d",
			len(original), len(in[0].Parts[0].Text))
	}
}

// TestCollapseEnvBlocks_OnlyAssistantTurns verifies that a conversation
// with no user turns at all (degenerate but possible during testing) is
// returned without panic.
func TestCollapseEnvBlocks_OnlyAssistantTurns(t *testing.T) {
	defer withCollapseEnv(t, true, 100)()
	in := []*genai.Content{
		mkTurn(genai.RoleModel, "a"),
		mkTurn(genai.RoleModel, "b"),
	}
	out := CollapseEnvBlocks(in)
	if len(out) != 2 {
		t.Fatalf("len(out)=%d; want 2", len(out))
	}
}
