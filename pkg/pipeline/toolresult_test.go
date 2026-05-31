package pipeline

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// withToolResult temporarily overrides the tool-result-truncation knobs.
func withToolResult(t *testing.T, enabled bool, max, head, tail int32) func() {
	t.Helper()
	pe, pm, ph, pt := toolResultTruncate, toolResultMaxBytes, toolResultHeadBytes, toolResultTailBytes
	toolResultTruncate, toolResultMaxBytes, toolResultHeadBytes, toolResultTailBytes = enabled, max, head, tail
	return func() {
		toolResultTruncate, toolResultMaxBytes, toolResultHeadBytes, toolResultTailBytes = pe, pm, ph, pt
	}
}

func TestTruncate_Disabled(t *testing.T) {
	defer withToolResult(t, false, 100, 40, 20)()
	in := []*genai.Content{
		mkTurn(genai.RoleUser, strings.Repeat("A", 5000)),
		mkTurn(genai.RoleModel, "ack"),
	}
	out := TruncateToolResults(in)
	if &out[0] != &in[0] {
		t.Error("expected fast-path return when disabled")
	}
}

func TestTruncate_MiddleElide(t *testing.T) {
	defer withToolResult(t, true, 200, 50, 30)()
	big := strings.Repeat("line of file\n", 500) // ~6.5KB, well over max
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big),    // turn 0 — older, eligible
		mkTurn(genai.RoleModel, "ack"), // turn 1
		mkTurn(genai.RoleUser, big),    // turn 2 — latest, exempt
	}
	out := TruncateToolResults(in)

	if len(out[0].Parts[0].Text) >= len(big) {
		t.Errorf("older turn was not truncated: len=%d", len(out[0].Parts[0].Text))
	}
	if !strings.Contains(out[0].Parts[0].Text, "bytes elided") {
		t.Errorf("older turn missing elision marker: %q", out[0].Parts[0].Text)
	}
	if out[2].Parts[0].Text != big {
		t.Errorf("latest turn must remain verbatim")
	}
}

func TestTruncate_NoMutation(t *testing.T) {
	defer withToolResult(t, true, 200, 50, 30)()
	big := strings.Repeat("x\n", 4000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big),
		mkTurn(genai.RoleModel, "ack"),
	}
	orig := in[0].Parts[0].Text
	_ = TruncateToolResults(in)
	if in[0].Parts[0].Text != orig {
		t.Error("input slice was mutated")
	}
}

func TestTruncate_SmallUntouched(t *testing.T) {
	defer withToolResult(t, true, 10000, 50, 30)()
	small := "short tool output"
	in := []*genai.Content{
		mkTurn(genai.RoleUser, small),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, "now"),
	}
	out := TruncateToolResults(in)
	if out[0].Parts[0].Text != small {
		t.Errorf("sub-threshold block was modified: %q", out[0].Parts[0].Text)
	}
}

func TestTruncate_FunctionResponse(t *testing.T) {
	defer withToolResult(t, true, 200, 50, 30)()
	big := strings.Repeat("data\n", 500)
	frTurn := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				Name:     "read_file",
				Response: map[string]any{"output": big, "count": float64(3)},
			},
		}},
	}
	in := []*genai.Content{
		frTurn,
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, "next"),
	}
	out := TruncateToolResults(in)
	got, _ := out[0].Parts[0].FunctionResponse.Response["output"].(string)
	if len(got) >= len(big) {
		t.Errorf("function response output not truncated: len=%d", len(got))
	}
	if out[0].Parts[0].FunctionResponse.Response["count"] != float64(3) {
		t.Error("non-text response field was disturbed")
	}
	// Original map must be intact (copy-on-write).
	if frTurn.Parts[0].FunctionResponse.Response["output"].(string) != big {
		t.Error("original FunctionResponse map mutated")
	}
}
