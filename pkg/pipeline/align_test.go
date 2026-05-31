package pipeline

import (
	"reflect"
	"testing"

	"google.golang.org/genai"
)

func TestAlignFunctionCallsAndResponses_NoModelTurns(t *testing.T) {
	in := []*genai.Content{
		{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{{Text: "Hello"}},
		},
	}
	out := AlignFunctionCallsAndResponses(in)
	if !reflect.DeepEqual(in, out) {
		t.Errorf("expected no changes when there are no model turns with function calls")
	}
}

func TestAlignFunctionCallsAndResponses_ModelTurnNoCalls(t *testing.T) {
	in := []*genai.Content{
		{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{{Text: "Hello"}},
		},
		{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: "Hi there!"}},
		},
	}
	out := AlignFunctionCallsAndResponses(in)
	if !reflect.DeepEqual(in, out) {
		t.Errorf("expected no changes when model turn has no function calls")
	}
}

func TestAlignFunctionCallsAndResponses_PerfectMatch(t *testing.T) {
	in := []*genai.Content{
		{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{
					FunctionCall: &genai.FunctionCall{
						ID:   "call_1",
						Name: "search",
					},
				},
				{
					FunctionCall: &genai.FunctionCall{
						ID:   "call_2",
						Name: "read_file",
					},
				},
			},
		},
		{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				{
					FunctionResponse: &genai.FunctionResponse{
						ID:       "call_1",
						Name:     "search",
						Response: map[string]any{"output": "search results"},
					},
				},
				{
					FunctionResponse: &genai.FunctionResponse{
						ID:       "call_2",
						Name:     "read_file",
						Response: map[string]any{"output": "file content"},
					},
				},
			},
		},
	}

	out := AlignFunctionCallsAndResponses(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(out))
	}

	// Verify the response parts match the input exactly
	nextParts := out[1].Parts
	if len(nextParts) != 2 {
		t.Fatalf("expected 2 response parts, got %d", len(nextParts))
	}
	if nextParts[0].FunctionResponse.ID != "call_1" || nextParts[1].FunctionResponse.ID != "call_2" {
		t.Errorf("perfect match: IDs changed or incorrect: %+v", nextParts)
	}
}

func TestAlignFunctionCallsAndResponses_MissingAndExtraAndTextPreserved(t *testing.T) {
	in := []*genai.Content{
		{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{
					FunctionCall: &genai.FunctionCall{
						ID:   "call_1",
						Name: "search",
					},
				},
				{
					FunctionCall: &genai.FunctionCall{
						ID:   "call_2",
						Name: "read_file",
					},
				},
			},
		},
		{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				{
					Text: "Some user text instructions",
				},
				{
					FunctionResponse: &genai.FunctionResponse{
						ID:       "call_2", // Matches call_2
						Name:     "read_file",
						Response: map[string]any{"output": "file content"},
					},
				},
				{
					FunctionResponse: &genai.FunctionResponse{
						ID:       "call_extra", // Unsolicited response
						Name:     "extra_tool",
						Response: map[string]any{"output": "unsolicited"},
					},
				},
			},
		},
	}

	out := AlignFunctionCallsAndResponses(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(out))
	}

	nextParts := out[1].Parts
	// We expect:
	// 1. Text part preserved
	// 2. Synthesized FunctionResponse for call_1
	// 3. Matched FunctionResponse for call_2
	// 4. The unsolicited call_extra response is dropped!
	if len(nextParts) != 3 {
		t.Fatalf("expected 3 parts (1 text + 2 responses), got %d: %+v", len(nextParts), nextParts)
	}

	if nextParts[0].Text != "Some user text instructions" {
		t.Errorf("expected text part at index 0, got %+v", nextParts[0])
	}

	fr1 := nextParts[1].FunctionResponse
	if fr1 == nil {
		t.Fatalf("expected function response at index 1")
	}
	if fr1.ID != "call_1" || fr1.Name != "search" {
		t.Errorf("expected call_1 response, got %+v", fr1)
	}
	if val, ok := fr1.Response["output"].(string); !ok || val != "omitted by client" {
		t.Errorf("expected dummy response 'omitted by client', got %+v", fr1.Response)
	}

	fr2 := nextParts[2].FunctionResponse
	if fr2 == nil {
		t.Fatalf("expected function response at index 2")
	}
	if fr2.ID != "call_2" || fr2.Name != "read_file" {
		t.Errorf("expected call_2 response, got %+v", fr2)
	}
	if val, ok := fr2.Response["output"].(string); !ok || val != "file content" {
		t.Errorf("expected preserved response 'file content', got %+v", fr2.Response)
	}
}

func TestAlignFunctionCallsAndResponses_FallbackToNameAndPopQueue(t *testing.T) {
	in := []*genai.Content{
		{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{
					FunctionCall: &genai.FunctionCall{
						ID:   "call_1",
						Name: "read_file",
					},
				},
				{
					FunctionCall: &genai.FunctionCall{
						ID:   "call_2",
						Name: "read_file",
					},
				},
			},
		},
		{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				{
					FunctionResponse: &genai.FunctionResponse{
						ID:       "", // No ID, match by Name "read_file"
						Name:     "read_file",
						Response: map[string]any{"output": "first file"},
					},
				},
				{
					FunctionResponse: &genai.FunctionResponse{
						ID:       "", // No ID, match by Name "read_file"
						Name:     "read_file",
						Response: map[string]any{"output": "second file"},
					},
				},
			},
		},
	}

	out := AlignFunctionCallsAndResponses(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(out))
	}

	nextParts := out[1].Parts
	if len(nextParts) != 2 {
		t.Fatalf("expected 2 response parts, got %d", len(nextParts))
	}

	fr1 := nextParts[0].FunctionResponse
	fr2 := nextParts[1].FunctionResponse

	if fr1 == nil || fr2 == nil {
		t.Fatalf("expected both response parts to be populated")
	}

	// Verify that the responses were popped sequentially and mapped properly to call_1 and call_2
	if fr1.Response["output"] != "first file" {
		t.Errorf("expected fr1 output to be 'first file', got %v", fr1.Response["output"])
	}
	if fr2.Response["output"] != "second file" {
		t.Errorf("expected fr2 output to be 'second file', got %v", fr2.Response["output"])
	}
}

func TestAlignFunctionCallsAndResponses_FallbackToNameWhenIDMismatchesButNameMatches(t *testing.T) {
	in := []*genai.Content{
		{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{
					FunctionCall: &genai.FunctionCall{
						ID:   "call_1",
						Name: "search",
					},
				},
			},
		},
		{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				{
					FunctionResponse: &genai.FunctionResponse{
						ID:       "different_id_somehow", // ID mismatches
						Name:     "search",               // Name matches!
						Response: map[string]any{"output": "recovered results"},
					},
				},
			},
		},
	}

	out := AlignFunctionCallsAndResponses(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(out))
	}

	nextParts := out[1].Parts
	if len(nextParts) != 1 {
		t.Fatalf("expected 1 response part, got %d", len(nextParts))
	}

	fr := nextParts[0].FunctionResponse
	if fr == nil {
		t.Fatalf("expected function response to be populated")
	}

	if fr.Response["output"] != "recovered results" {
		t.Errorf("expected output to fall back to name and match, got %v", fr.Response["output"])
	}
}
