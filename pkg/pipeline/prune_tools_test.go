package pipeline

import (
	"testing"

	"google.golang.org/genai"
)

func withPruneTools(t *testing.T, enabled bool) func() {
	t.Helper()
	prev := pruneStaleTools
	pruneStaleTools = enabled
	return func() { pruneStaleTools = prev }
}

// callTurn builds a single-part assistant FunctionCall turn.
func callTurn(name string, args map[string]any) *genai.Content {
	return &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}},
	}
}

// respTurn builds a single-part user FunctionResponse turn.
func respTurn(name string, resp map[string]any) *genai.Content {
	return &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: name, Response: resp}}},
	}
}

func TestPrune_Disabled(t *testing.T) {
	defer withPruneTools(t, false)()
	in := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "start"}}},
		callTurn("read_file", map[string]any{"path": "a.go"}),
		respTurn("read_file", map[string]any{"output": "v1"}),
		callTurn("read_file", map[string]any{"path": "a.go"}),
		respTurn("read_file", map[string]any{"output": "v2"}),
	}
	out := PruneStaleTools(in)
	if len(out) != len(in) {
		t.Fatalf("expected same slice length, got %d", len(out))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("expected fast-path pointer equality at index %d when disabled", i)
		}
	}
}

func TestPrune_SupersededReadFile(t *testing.T) {
	defer withPruneTools(t, true)()
	in := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "start"}}},                         // 0 kept
		callTurn("read_file", map[string]any{"path": "a.go"}),                                 // 1 pruned (part replaced)
		respTurn("read_file", map[string]any{"output": "v1"}),                                 // 2 pruned (part replaced)
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "thinking"}}},                     // 3 kept
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "again"}}},                         // 4 kept
		callTurn("read_file", map[string]any{"path": "a.go"}),                                 // 5 kept (latest)
		respTurn("read_file", map[string]any{"output": "v2"}),                                 // 6 kept
	}

	out := PruneStaleTools(in)
	if len(out) != len(in) {
		t.Fatalf("expected identical length %d, got %d", len(in), len(out))
	}

	// Turn 1's FunctionCall should be replaced by a text placeholder
	if out[1].Parts[0].FunctionCall != nil || out[1].Parts[0].Text != "(superseded read_file call pruned)" {
		t.Errorf("expected turn 1 part to be placeholder, got %+v", out[1].Parts[0])
	}

	// Turn 2's FunctionResponse should be replaced by a text placeholder
	if out[2].Parts[0].FunctionResponse != nil || out[2].Parts[0].Text != "(superseded read_file output pruned)" {
		t.Errorf("expected turn 2 part to be placeholder, got %+v", out[2].Parts[0])
	}

	// Turn 5 and 6 should be left completely intact (latest)
	if out[5].Parts[0].FunctionCall == nil || out[5].Parts[0].FunctionCall.Name != "read_file" {
		t.Errorf("expected turn 5 FunctionCall to be intact, got %+v", out[5].Parts[0])
	}
	if out[6].Parts[0].FunctionResponse == nil || out[6].Parts[0].FunctionResponse.Response["output"] != "v2" {
		t.Errorf("expected turn 6 FunctionResponse to hold v2, got %+v", out[6].Parts[0])
	}
}

func TestPrune_MutatingToolKept(t *testing.T) {
	defer withPruneTools(t, true)()
	in := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "start"}}},
		callTurn("write_to_file", map[string]any{"path": "a.go"}),
		respTurn("write_to_file", map[string]any{"output": "ok1"}),
		callTurn("write_to_file", map[string]any{"path": "a.go"}),
		respTurn("write_to_file", map[string]any{"output": "ok2"}),
	}
	out := PruneStaleTools(in)
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("mutating tools should never be replaced/mutated, index %d changed", i)
		}
	}
}

func TestPrune_DifferentArgsKept(t *testing.T) {
	defer withPruneTools(t, true)()
	in := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "start"}}},
		callTurn("read_file", map[string]any{"path": "a.go"}),
		respTurn("read_file", map[string]any{"output": "a"}),
		callTurn("read_file", map[string]any{"path": "b.go"}),
		respTurn("read_file", map[string]any{"output": "b"}),
	}
	out := PruneStaleTools(in)
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("distinct-arg reads should never be replaced, index %d changed", i)
		}
	}
}

func TestPrune_NoMutation(t *testing.T) {
	defer withPruneTools(t, true)()
	in := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "start"}}},
		callTurn("read_file", map[string]any{"path": "a.go"}),
		respTurn("read_file", map[string]any{"output": "v1"}),
		callTurn("read_file", map[string]any{"path": "a.go"}),
		respTurn("read_file", map[string]any{"output": "v2"}),
	}
	_ = PruneStaleTools(in)
	if in[1].Parts[0].FunctionCall == nil || in[1].Parts[0].FunctionCall.Name != "read_file" {
		t.Error("input slice elements or their parts were mutated in-place")
	}
}

func TestPrune_MultiPartAgent(t *testing.T) {
	defer withPruneTools(t, true)()

	in := []*genai.Content{
		{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{{Text: "initial user prompt"}},
		},
		{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{Text: "thinking about running first read_file..."}, // index 0 (keep)
				{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": "main.go"}}}, // index 1 (prune)
			},
		},
		{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				{Text: "<environment_details>os=linux</environment_details>"}, // index 0 (keep)
				{FunctionResponse: &genai.FunctionResponse{Name: "read_file", Response: map[string]any{"output": "package main..."}}}, // index 1 (prune)
			},
		},
		{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{Text: "thinking about running second read_file..."}, // index 0 (keep)
				{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": "main.go"}}}, // index 1 (keep - latest)
			},
		},
		{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				{Text: "<environment_details>os=linux</environment_details>"}, // index 0 (keep)
				{FunctionResponse: &genai.FunctionResponse{Name: "read_file", Response: map[string]any{"output": "package main\n// updated"}}}, // index 1 (keep - latest)
			},
		},
	}

	out := PruneStaleTools(in)
	if len(out) != len(in) {
		t.Fatalf("expected identical outer slice length, got %d", len(out))
	}

	// Model turn 1: First part (thoughts) must be intact, second part (call) must be pruned
	m1 := out[1]
	if len(m1.Parts) != 2 {
		t.Fatalf("expected model turn 1 to have 2 parts, got %d", len(m1.Parts))
	}
	if m1.Parts[0].Text != "thinking about running first read_file..." {
		t.Errorf("expected model thoughts to be preserved, got %q", m1.Parts[0].Text)
	}
	if m1.Parts[1].FunctionCall != nil || m1.Parts[1].Text != "(superseded read_file call pruned)" {
		t.Errorf("expected model FunctionCall to be placeholder, got %+v", m1.Parts[1])
	}

	// User turn 2: First part (env details) must be intact, second part (response) must be pruned
	u2 := out[2]
	if len(u2.Parts) != 2 {
		t.Fatalf("expected user turn 2 to have 2 parts, got %d", len(u2.Parts))
	}
	if u2.Parts[0].Text != "<environment_details>os=linux</environment_details>" {
		t.Errorf("expected env details to be preserved, got %q", u2.Parts[0].Text)
	}
	if u2.Parts[1].FunctionResponse != nil || u2.Parts[1].Text != "(superseded read_file output pruned)" {
		t.Errorf("expected user FunctionResponse to be placeholder, got %+v", u2.Parts[1])
	}

	// Model turn 3: Both parts must be untouched (latest call)
	m3 := out[3]
	if m3.Parts[1].FunctionCall == nil || m3.Parts[1].FunctionCall.Name != "read_file" {
		t.Errorf("expected latest model turn call to be untouched, got %+v", m3.Parts[1])
	}

	// User turn 4: Both parts must be untouched (latest response)
	u4 := out[4]
	if u4.Parts[1].FunctionResponse == nil || u4.Parts[1].FunctionResponse.Name != "read_file" {
		t.Errorf("expected latest user response to be untouched, got %+v", u4.Parts[1])
	}
}
