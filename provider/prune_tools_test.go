package provider

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
		mkTurn(genai.RoleUser, "start"),
		callTurn("read_file", map[string]any{"path": "a.go"}),
		respTurn("read_file", map[string]any{"output": "v1"}),
		callTurn("read_file", map[string]any{"path": "a.go"}),
		respTurn("read_file", map[string]any{"output": "v2"}),
	}
	out := PruneStaleTools(in)
	if len(out) != len(in) {
		t.Errorf("expected no-op when disabled, got len %d", len(out))
	}
}

func TestPrune_SupersededReadFile(t *testing.T) {
	defer withPruneTools(t, true)()
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "start"),                       // 0 kept
		callTurn("read_file", map[string]any{"path": "a.go"}), // 1 dropped
		respTurn("read_file", map[string]any{"output": "v1"}), // 2 dropped
		mkTurn(genai.RoleModel, "thinking"),                   // 3 kept
		mkTurn(genai.RoleUser, "again"),                       // 4 kept
		callTurn("read_file", map[string]any{"path": "a.go"}), // 5 kept (latest)
		respTurn("read_file", map[string]any{"output": "v2"}), // 6 kept
	}
	out := PruneStaleTools(in)
	if len(out) != 5 {
		t.Fatalf("expected 5 turns after prune, got %d", len(out))
	}
	// The remaining read_file response should hold v2.
	foundV2 := false
	for _, c := range out {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Response["output"] == "v2" {
				foundV2 = true
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Response["output"] == "v1" {
				t.Error("superseded v1 response was retained")
			}
		}
	}
	if !foundV2 {
		t.Error("latest v2 read was dropped")
	}
}

func TestPrune_MutatingToolKept(t *testing.T) {
	defer withPruneTools(t, true)()
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "start"),
		callTurn("write_to_file", map[string]any{"path": "a.go"}),
		respTurn("write_to_file", map[string]any{"output": "ok1"}),
		callTurn("write_to_file", map[string]any{"path": "a.go"}),
		respTurn("write_to_file", map[string]any{"output": "ok2"}),
	}
	out := PruneStaleTools(in)
	if len(out) != len(in) {
		t.Errorf("mutating tool exchanges must never be pruned, got len %d", len(out))
	}
}

func TestPrune_DifferentArgsKept(t *testing.T) {
	defer withPruneTools(t, true)()
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "start"),
		callTurn("read_file", map[string]any{"path": "a.go"}),
		respTurn("read_file", map[string]any{"output": "a"}),
		callTurn("read_file", map[string]any{"path": "b.go"}),
		respTurn("read_file", map[string]any{"output": "b"}),
	}
	out := PruneStaleTools(in)
	if len(out) != len(in) {
		t.Errorf("distinct-arg reads must be kept, got len %d", len(out))
	}
}

func TestPrune_NoMutation(t *testing.T) {
	defer withPruneTools(t, true)()
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "start"),
		callTurn("read_file", map[string]any{"path": "a.go"}),
		respTurn("read_file", map[string]any{"output": "v1"}),
		callTurn("read_file", map[string]any{"path": "a.go"}),
		respTurn("read_file", map[string]any{"output": "v2"}),
	}
	origLen := len(in)
	_ = PruneStaleTools(in)
	if len(in) != origLen {
		t.Error("input slice length changed")
	}
	if in[1].Parts[0].FunctionCall == nil {
		t.Error("input slice element mutated")
	}
}
