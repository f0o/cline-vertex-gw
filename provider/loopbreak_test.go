package provider

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// withLoopBreak knobs helper to override variables during tests
func withLoopBreak(t *testing.T, enabled, nudge bool) func() {
	t.Helper()
	prevEnabled := breakLoopTrapEnabled
	prevNudge := loopTrapNudgeEnabled
	breakLoopTrapEnabled = enabled
	loopTrapNudgeEnabled = nudge
	return func() {
		breakLoopTrapEnabled = prevEnabled
		loopTrapNudgeEnabled = prevNudge
	}
}

// TestBreakLoopTrap_Disabled verifies fast-path return when disabled
func TestBreakLoopTrap_Disabled(t *testing.T) {
	defer withLoopBreak(t, false, true)()

	in := []*genai.Content{
		mkTurn(genai.RoleUser, "You did not use a tool in your previous response! Please retry with a tool use."),
		mkTurn(genai.RoleModel, ""),
		mkTurn(genai.RoleUser, "You did not use a tool in your previous response! Please retry with a tool use."),
	}

	out := BreakLoopTrap(in)
	if len(out) != len(in) {
		t.Errorf("expected fast-path when disabled; len(out)=%d, want %d", len(out), len(in))
	}
}

// TestBreakLoopTrap_NoScoldings verifies no modifications when no scoldings are present
func TestBreakLoopTrap_NoScoldings(t *testing.T) {
	defer withLoopBreak(t, true, true)()

	in := []*genai.Content{
		mkTurn(genai.RoleUser, "hello"),
		mkTurn(genai.RoleModel, "hi"),
		mkTurn(genai.RoleUser, "how are you?"),
	}

	out := BreakLoopTrap(in)
	if len(out) != len(in) {
		t.Errorf("expected no changes; len(out)=%d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Parts[0].Text != in[i].Parts[0].Text {
			t.Errorf("turn %d modified; got %q, want %q", i, out[i].Parts[0].Text, in[i].Parts[0].Text)
		}
	}
}

// TestBreakLoopTrap_Deduplication verifies duplicate scoldings and empty turns are dropped, keeping alternation
func TestBreakLoopTrap_Deduplication(t *testing.T) {
	defer withLoopBreak(t, true, false)() // keep nudge off for clean comparison

	in := []*genai.Content{
		mkTurn(genai.RoleUser, "read file"),        // 0: Keep
		mkTurn(genai.RoleModel, "reading file..."), // 1: Keep
		mkTurn(genai.RoleUser, "You did not use a tool in your previous response! Please retry with a tool use."), // 2: Drop (duplicate of 6)
		mkTurn(genai.RoleModel, ""), // 3: Drop (trailing empty model)
		mkTurn(genai.RoleUser, "TODO LIST UPDATE REQUIRED - You MUST include the task_progress parameter"), // 4: Drop (duplicate of 8)
		mkTurn(genai.RoleModel, ""), // 5: Drop (trailing empty model)
		mkTurn(genai.RoleUser, "You did not use a tool in your previous response! Please retry with a tool use."), // 6: Keep (most recent no-tool)
		mkTurn(genai.RoleModel, "some action"), // 7: Keep (non-empty model)
		mkTurn(genai.RoleUser, "TODO LIST UPDATE REQUIRED - You MUST include the task_progress parameter"), // 8: Keep (most recent todo)
	}

	out := BreakLoopTrap(in)
	// Expected kept indices from 'in': 0, 1, 6, 7, 8
	expectedLen := 5
	if len(out) != expectedLen {
		t.Fatalf("len(out)=%d; want %d", len(out), expectedLen)
	}

	if out[0].Parts[0].Text != "read file" {
		t.Errorf("turn 0 text = %q; want 'read file'", out[0].Parts[0].Text)
	}
	if out[1].Parts[0].Text != "reading file..." {
		t.Errorf("turn 1 text = %q; want 'reading file...'", out[1].Parts[0].Text)
	}
	if !strings.Contains(out[2].Parts[0].Text, "You did not use a tool") {
		t.Errorf("turn 2 text = %q; want no-tool scolding", out[2].Parts[0].Text)
	}
	if out[3].Parts[0].Text != "some action" {
		t.Errorf("turn 3 text = %q; want 'some action'", out[3].Parts[0].Text)
	}
	if !strings.Contains(out[4].Parts[0].Text, "TODO LIST UPDATE REQUIRED") {
		t.Errorf("turn 4 text = %q; want todo scolding", out[4].Parts[0].Text)
	}
}

// TestBreakLoopTrap_Nudge verifies action placeholder nudge is appended to the latest scolding user turn
func TestBreakLoopTrap_Nudge(t *testing.T) {
	defer withLoopBreak(t, true, true)()

	in := []*genai.Content{
		mkTurn(genai.RoleUser, "some request"),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, "You did not use a tool in your previous response! Please retry with a tool use."),
	}

	out := BreakLoopTrap(in)
	if len(out) != 3 {
		t.Fatalf("len(out)=%d; want 3", len(out))
	}

	lastText := out[2].Parts[0].Text
	if !strings.Contains(lastText, "If you have no other tools to call, please execute 'pytest'") {
		t.Errorf("nudge not found in latest user turn text: %q", lastText)
	}
}

// TestBreakLoopTrap_NudgeDisabled verifies nudge is NOT appended when disabled
func TestBreakLoopTrap_NudgeDisabled(t *testing.T) {
	defer withLoopBreak(t, true, false)()

	in := []*genai.Content{
		mkTurn(genai.RoleUser, "some request"),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, "You did not use a tool in your previous response! Please retry with a tool use."),
	}

	out := BreakLoopTrap(in)
	if len(out) != 3 {
		t.Fatalf("len(out)=%d; want 3", len(out))
	}

	lastText := out[2].Parts[0].Text
	if strings.Contains(lastText, "If you have no other tools to call") {
		t.Errorf("nudge appended when disabled: %q", lastText)
	}
}

func TestBreakLoopTrap_Safety_FirstTurn(t *testing.T) {
	defer withLoopBreak(t, true, false)()

	in := []*genai.Content{
		// Turn 0: User turn containing the scolding/rule, but it's the FIRST message (i == 0)
		mkTurn(genai.RoleUser, "You MUST include the task_progress parameter for this assignment."),
		mkTurn(genai.RoleModel, "ack"),
		// Turn 2: Another user turn containing the same rule (duplicate scolding!)
		mkTurn(genai.RoleUser, "You MUST include the task_progress parameter for this assignment."),
	}

	out := BreakLoopTrap(in)
	// Even though Turn 0 is technically a duplicate scolding, it should NOT be dropped because i == 0!
	// So all 3 turns should be kept.
	if len(out) != 3 {
		t.Errorf("expected all 3 turns to be kept; len(out)=%d, want 3", len(out))
	}
}

func TestBreakLoopTrap_Safety_FunctionResponse(t *testing.T) {
	defer withLoopBreak(t, true, false)()

	// Let's build a user turn that contains a tool result but also the scolding text
	userTurnWithToolResponse := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			{FunctionResponse: &genai.FunctionResponse{Name: "execute_command", ID: "call_1", Response: map[string]any{"output": "success"}}},
			{Text: "TODO LIST UPDATE REQUIRED - You MUST include the task_progress parameter"},
		},
	}

	in := []*genai.Content{
		mkTurn(genai.RoleUser, "some initial task"),
		mkTurn(genai.RoleModel, "ack"),
		userTurnWithToolResponse, // Turn 2: technically matches scolding but contains FunctionResponse!
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, "TODO LIST UPDATE REQUIRED - You MUST include the task_progress parameter"), // Turn 4: the last scolding turn of its type
	}

	out := BreakLoopTrap(in)
	// Turn 2 should NOT be dropped because it contains a FunctionResponse.
	// So we should keep turns: 0, 1, 2, 3, 4. Length should be 5.
	if len(out) != 5 {
		t.Errorf("expected turn with FunctionResponse to be protected; len(out)=%d, want 5", len(out))
	}
}

func TestBreakLoopTrap_Safety_FunctionCall(t *testing.T) {
	defer withLoopBreak(t, true, false)()

	modelTurnWithToolCall := &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{Name: "read_file", ID: "call_2", Args: map[string]any{"path": "go.mod"}}},
		},
	}

	in := []*genai.Content{
		mkTurn(genai.RoleUser, "some task"),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, "You did not use a tool in your previous response! Please retry with a tool use."), // Turn 2: duplicate scolding
		modelTurnWithToolCall, // Turn 3: trailing model turn but it contains a FunctionCall!
		mkTurn(genai.RoleUser, "You did not use a tool in your previous response! Please retry with a tool use."), // Turn 4: last scolding turn
	}

	out := BreakLoopTrap(in)
	// Turn 2 and 3 should NOT be dropped because Turn 3 contains a FunctionCall!
	// So we keep all 5 turns.
	if len(out) != 5 {
		t.Errorf("expected model turn with FunctionCall and its preceding scolding to be protected; len(out)=%d, want 5", len(out))
	}
}
