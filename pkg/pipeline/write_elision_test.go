package pipeline

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// withWriteElision temporarily overrides the write-action elision knobs.
func withWriteElision(t *testing.T, enabled bool) func() {
	t.Helper()
	prev := writeActionElision
	writeActionElision = enabled
	return func() {
		writeActionElision = prev
	}
}

func TestWriteElision_Disabled(t *testing.T) {
	defer withWriteElision(t, false)()

	big := strings.Repeat("huge file content\n", 500) // ~9KB

	in := []*genai.Content{
		{
			Role: genai.RoleModel,
			Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					Name: "write_to_file",
					Args: map[string]any{"path": "foo.txt", "content": big},
				},
			}},
		},
		mkTurn(genai.RoleUser, "ack"),
		mkTurn(genai.RoleModel, "done"),
	}

	out := ElideHistoricalWriteActions(in)
	if &out[0] != &in[0] {
		t.Error("expected fast-path return when disabled")
	}
}

func TestWriteElision_ElidesHistorical(t *testing.T) {
	defer withWriteElision(t, true)()

	big := strings.Repeat("this is some very large file contents that we wrote\n", 150) // ~2.5KB

	in := []*genai.Content{
		{ // Turn 0: Model tool-call (Older than 2 turns, should be elided)
			Role: genai.RoleModel,
			Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   "call_1",
					Name: "write_to_file",
					Args: map[string]any{"path": "foo.txt", "content": big},
				},
			}},
		},
		{ // Turn 1: User response to tool call (Within 2 turns, should remain untouched)
			Role: genai.RoleUser,
			Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{
					Name:     "write_to_file",
					Response: map[string]any{"result": "success"},
				},
			}},
		},
		{ // Turn 2: Model edit tool-call (Within 2 turns, should remain untouched)
			Role: genai.RoleModel,
			Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   "call_2",
					Name: "replace_in_file",
					Args: map[string]any{"path": "foo.txt", "diff": big},
				},
			}},
		},
		mkTurn(genai.RoleUser, "the actual latest turn"), // Turn 3: Latest (Exempt)
	}

	out := ElideHistoricalWriteActions(in)

	// Turn 3: must be untouched
	if out[3].Parts[0].Text != "the actual latest turn" {
		t.Errorf("latest turn modified: %s", out[3].Parts[0].Text)
	}

	// Turn 2: should NOT be elided because distance is 1 (3-2 = 1 < 2)
	got2, _ := out[2].Parts[0].FunctionCall.Args["diff"].(string)
	if got2 != big {
		t.Errorf("turn 2 was prematurely elided: len=%d", len(got2))
	}

	// Turn 0: should BE elided because distance is 3 (3-0 = 3 >= 2)
	got0, _ := out[0].Parts[0].FunctionCall.Args["content"].(string)
	if !strings.Contains(got0, "Content:") || !strings.Contains(got0, "Elided. Retrieve full content: hash=") {
		t.Errorf("turn 0 content was not correctly elided: %q", got0)
	}

	// Double check ID and other fields in Turn 0 remain intact
	if out[0].Parts[0].FunctionCall.ID != "call_1" {
		t.Errorf("Turn 0 FunctionCall ID modified: %s", out[0].Parts[0].FunctionCall.ID)
	}
	if out[0].Parts[0].FunctionCall.Args["path"] != "foo.txt" {
		t.Errorf("Turn 0 path argument modified: %v", out[0].Parts[0].FunctionCall.Args["path"])
	}

	// Original input must not be mutated
	orig, _ := in[0].Parts[0].FunctionCall.Args["content"].(string)
	if orig != big {
		t.Error("original input content mutated")
	}
}

func TestWriteElision_SmallUntouched(t *testing.T) {
	defer withWriteElision(t, true)()

	small := "small contents"

	in := []*genai.Content{
		{
			Role: genai.RoleModel,
			Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					Name: "write_to_file",
					Args: map[string]any{"path": "foo.txt", "content": small},
				},
			}},
		},
		mkTurn(genai.RoleUser, "ack"),
		mkTurn(genai.RoleModel, "done"),
	}

	out := ElideHistoricalWriteActions(in)
	got, _ := out[0].Parts[0].FunctionCall.Args["content"].(string)
	if got != small {
		t.Errorf("small historical payload was modified: %q", got)
	}
}
