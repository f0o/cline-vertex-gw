package provider

import (
	"encoding/json"
	"testing"

	"google.golang.org/genai"
)

func TestMarshalToolArgs(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		got, err := MarshalToolArgs(nil)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got != "{}" {
			t.Errorf("got %q; want {}", got)
		}
	})
	t.Run("empty map", func(t *testing.T) {
		got, err := MarshalToolArgs(map[string]any{})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got != "{}" {
			t.Errorf("got %q; want {}", got)
		}
	})
	t.Run("populated", func(t *testing.T) {
		got, err := MarshalToolArgs(map[string]any{"path": "/tmp/x"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		var back map[string]any
		if err := json.Unmarshal([]byte(got), &back); err != nil {
			t.Fatalf("round-trip parse: %v (got %q)", err, got)
		}
		if back["path"] != "/tmp/x" {
			t.Errorf("round-trip lost data: %v", back)
		}
	})
}

func TestUnmarshalToolArgs(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		args, err := UnmarshalToolArgs("")
		if err != nil {
			t.Errorf("err = %v; want nil for empty input", err)
		}
		if args != nil {
			t.Errorf("args = %v; want nil for empty input", args)
		}
	})
	t.Run("valid JSON", func(t *testing.T) {
		args, err := UnmarshalToolArgs(`{"path":"main.go"}`)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if args["path"] != "main.go" {
			t.Errorf("args[path] = %v; want main.go", args["path"])
		}
	})
	t.Run("malformed yields error not panic", func(t *testing.T) {
		_, err := UnmarshalToolArgs(`{"path": main.go}`) // invalid JSON
		if err == nil {
			t.Errorf("expected an error for malformed JSON, got nil")
		}
	})
}

func TestFunctionResponseToText(t *testing.T) {
	cases := map[string]struct {
		fr   *genai.FunctionResponse
		want string
	}{
		"nil": {nil, ""},
		"empty response": {
			&genai.FunctionResponse{Response: map[string]any{}}, "",
		},
		"output string": {
			&genai.FunctionResponse{Response: map[string]any{"output": "result"}},
			"result",
		},
		"output structured": {
			&genai.FunctionResponse{Response: map[string]any{"output": []any{1, 2, 3}}},
			"[1,2,3]",
		},
		"error fallback": {
			&genai.FunctionResponse{Response: map[string]any{"error": "boom"}},
			"boom",
		},
		"whole-object fallback": {
			&genai.FunctionResponse{Response: map[string]any{"foo": "bar"}},
			`{"foo":"bar"}`,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := FunctionResponseToText(c.fr); got != c.want {
				t.Errorf("got %q; want %q", got, c.want)
			}
		})
	}
}

// TestBuildAnthropicRequest_ToolDefsTranslated verifies that genai.Tool
// definitions surface as the correct Anthropic `tools` request array shape,
// preserving name/description/input_schema verbatim.
func TestBuildAnthropicRequest_ToolDefsTranslated(t *testing.T) {
	tools := []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        "read_file",
			Description: "Read a file from disk",
			ParametersJsonSchema: json.RawMessage(
				`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
	}}
	opts := &GenerationOptions{Tools: tools}
	contents := []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: "do it"}},
	}}
	req := buildAnthropicRequest("claude-3-5-sonnet", "", contents, opts, false)

	if len(req.Tools) != 1 {
		t.Fatalf("Tools len = %d; want 1", len(req.Tools))
	}
	tool := req.Tools[0]
	if tool.Name != "read_file" {
		t.Errorf("Name = %q; want read_file", tool.Name)
	}
	if tool.Description != "Read a file from disk" {
		t.Errorf("Description = %q", tool.Description)
	}
	// input_schema should be the raw JSON Schema we provided.
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatalf("input_schema not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("schema.type = %v; want object", schema["type"])
	}
}

// TestBuildAnthropicRequest_AssistantToolCallSurvives verifies that a prior
// assistant turn carrying a FunctionCall part round-trips to the Anthropic
// tool_use block on the wire (the bug fix that motivated the whole effort).
func TestBuildAnthropicRequest_AssistantToolCallSurvives(t *testing.T) {
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "please call read_file"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				ID:   "toolu_abc",
				Name: "read_file",
				Args: map[string]any{"path": "main.go"},
			},
		}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       "toolu_abc",
				Name:     "read_file",
				Response: map[string]any{"output": "package main"},
			},
		}}},
	}
	req := buildAnthropicRequest("claude-3-5-sonnet", "", contents, nil, false)
	if len(req.Messages) != 3 {
		t.Fatalf("Messages len = %d; want 3", len(req.Messages))
	}

	// Assistant turn must carry a tool_use block, not a plain string.
	assistant := req.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Blocks) != 1 {
		t.Fatalf("assistant turn = %+v; want one tool_use block", assistant)
	}
	if assistant.Blocks[0].Type != "tool_use" {
		t.Errorf("block type = %q; want tool_use", assistant.Blocks[0].Type)
	}
	if assistant.Blocks[0].ID != "toolu_abc" {
		t.Errorf("block id = %q; want toolu_abc", assistant.Blocks[0].ID)
	}
	if assistant.Blocks[0].Name != "read_file" {
		t.Errorf("block name = %q; want read_file", assistant.Blocks[0].Name)
	}
	var inputMap map[string]any
	if err := json.Unmarshal(assistant.Blocks[0].Input, &inputMap); err != nil {
		t.Fatalf("input not valid JSON: %v", err)
	}
	if inputMap["path"] != "main.go" {
		t.Errorf("input[path] = %v; want main.go", inputMap["path"])
	}

	// Tool result turn must carry a tool_result block.
	resultTurn := req.Messages[2]
	if resultTurn.Role != "user" || len(resultTurn.Blocks) != 1 {
		t.Fatalf("tool-result turn = %+v; want one tool_result block", resultTurn)
	}
	if resultTurn.Blocks[0].Type != "tool_result" {
		t.Errorf("block type = %q; want tool_result", resultTurn.Blocks[0].Type)
	}
	if resultTurn.Blocks[0].ToolUseID != "toolu_abc" {
		t.Errorf("tool_use_id = %q; want toolu_abc", resultTurn.Blocks[0].ToolUseID)
	}
}

func TestMapAnthropicStopReason_ToolUse(t *testing.T) {
	if got := mapAnthropicStopReason("tool_use"); got != FinishReasonToolCalls {
		t.Errorf("mapAnthropicStopReason(tool_use) = %q; want %q",
			got, FinishReasonToolCalls)
	}
}

func TestAnthropicResponseToGenaiParts_ToolUseBlock(t *testing.T) {
	blocks := []anthropicResponseBlock{
		{Type: "text", Text: "Let me check that file."},
		{
			Type:  "tool_use",
			ID:    "toolu_xyz",
			Name:  "read_file",
			Input: json.RawMessage(`{"path":"main.go"}`),
		},
	}
	parts := anthropicResponseToGenaiParts(blocks)
	if len(parts) != 2 {
		t.Fatalf("parts len = %d; want 2", len(parts))
	}
	if parts[0].Text != "Let me check that file." {
		t.Errorf("first part text = %q", parts[0].Text)
	}
	if parts[1].FunctionCall == nil {
		t.Fatalf("second part missing FunctionCall: %+v", parts[1])
	}
	fc := parts[1].FunctionCall
	if fc.ID != "toolu_xyz" || fc.Name != "read_file" {
		t.Errorf("FunctionCall = %+v; want toolu_xyz/read_file", fc)
	}
	if fc.Args["path"] != "main.go" {
		t.Errorf("Args[path] = %v; want main.go", fc.Args["path"])
	}
}

// TestTranslateToolConfigToAnthropic exercises the tool_choice mapping.
func TestTranslateToolConfigToAnthropic(t *testing.T) {
	cases := []struct {
		name         string
		cfg          *genai.ToolConfig
		wantChoice   *anthropicToolChoice
		wantSuppress bool
	}{
		{"nil", nil, nil, false},
		{"auto", &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingConfigModeAuto,
			},
		}, nil, false},
		{"any-no-allowed", &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingConfigModeAny,
			},
		}, &anthropicToolChoice{Type: "any"}, false},
		{"any-pinned", &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode:                 genai.FunctionCallingConfigModeAny,
				AllowedFunctionNames: []string{"read_file"},
			},
		}, &anthropicToolChoice{Type: "tool", Name: "read_file"}, false},
		{"none", &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingConfigModeNone,
			},
		}, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotChoice, gotSuppress := translateToolConfigToAnthropic(c.cfg)
			if (gotChoice == nil) != (c.wantChoice == nil) {
				t.Fatalf("choice = %v; want %v", gotChoice, c.wantChoice)
			}
			if gotChoice != nil && c.wantChoice != nil {
				if gotChoice.Type != c.wantChoice.Type || gotChoice.Name != c.wantChoice.Name {
					t.Errorf("choice = %+v; want %+v", gotChoice, c.wantChoice)
				}
			}
			if gotSuppress != c.wantSuppress {
				t.Errorf("suppress = %v; want %v", gotSuppress, c.wantSuppress)
			}
		})
	}
}

// TestBuildOpenAIRequest_ToolDefsTranslated verifies the OpenAI-compat
// adapter emits the standard tools+function shape.
func TestBuildOpenAIRequest_ToolDefsTranslated(t *testing.T) {
	tools := []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        "list_files",
			Description: "List files in a dir",
			ParametersJsonSchema: json.RawMessage(
				`{"type":"object","properties":{"dir":{"type":"string"}}}`),
		}},
	}}
	opts := &GenerationOptions{Tools: tools}
	contents := []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: "go"}},
	}}
	req := buildOpenAIRequest("llama-3.3-70b-instruct-maas", "", contents, opts, false)
	if len(req.Tools) != 1 {
		t.Fatalf("Tools len = %d; want 1", len(req.Tools))
	}
	if req.Tools[0].Type != "function" {
		t.Errorf("Type = %q; want function", req.Tools[0].Type)
	}
	if req.Tools[0].Function.Name != "list_files" {
		t.Errorf("Function.Name = %q; want list_files", req.Tools[0].Function.Name)
	}
}

// TestBuildOpenAIRequest_AssistantToolCallSurvives verifies that prior
// assistant tool_calls turn into the right wire shape.
func TestBuildOpenAIRequest_AssistantToolCallSurvives(t *testing.T) {
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "call list_files"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				ID:   "call_1",
				Name: "list_files",
				Args: map[string]any{"dir": "/tmp"},
			},
		}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       "call_1",
				Name:     "list_files",
				Response: map[string]any{"output": "a.txt\nb.txt"},
			},
		}}},
	}
	req := buildOpenAIRequest("llama-3.3-70b-instruct-maas", "", contents, nil, false)
	// We expect: user / assistant(tool_calls) / tool — three messages.
	if len(req.Messages) != 3 {
		t.Fatalf("Messages len = %d; want 3; got %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[1].Role != "assistant" || len(req.Messages[1].ToolCalls) != 1 {
		t.Fatalf("assistant turn = %+v; want one tool_call", req.Messages[1])
	}
	tc := req.Messages[1].ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "list_files" {
		t.Errorf("tool_call = %+v", tc)
	}
	// Arguments is the JSON-encoded STRING form on the wire.
	if tc.Function.Arguments != `{"dir":"/tmp"}` {
		t.Errorf("Arguments = %q; want stringified JSON", tc.Function.Arguments)
	}
	// Tool result turn.
	if req.Messages[2].Role != "tool" || req.Messages[2].ToolCallID != "call_1" {
		t.Errorf("tool turn = %+v; want role:tool tool_call_id:call_1", req.Messages[2])
	}
}

// TestBuildCohereRequest_ToolDefsTranslated verifies tool def translation to
// Cohere's flat parameter_definitions shape.
func TestBuildCohereRequest_ToolDefsTranslated(t *testing.T) {
	tools := []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        "search",
			Description: "Search the web",
			ParametersJsonSchema: json.RawMessage(
				`{"type":"object",
				  "properties":{
				    "q":{"type":"string","description":"query"},
				    "n":{"type":"integer","description":"results"}
				  },
				  "required":["q"]}`),
		}},
	}}
	opts := &GenerationOptions{Tools: tools}
	contents := []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: "go"}},
	}}
	req := buildCohereRequest("", contents, opts, false)
	if len(req.Tools) != 1 {
		t.Fatalf("Tools len = %d; want 1", len(req.Tools))
	}
	tool := req.Tools[0]
	if tool.Name != "search" {
		t.Errorf("Name = %q", tool.Name)
	}
	qDef, ok := tool.ParameterDefinitions["q"]
	if !ok {
		t.Fatalf("missing q param def")
	}
	if qDef.Type != "str" {
		t.Errorf("q.Type = %q; want str", qDef.Type)
	}
	if !qDef.Required {
		t.Errorf("q.Required = false; want true")
	}
	nDef := tool.ParameterDefinitions["n"]
	if nDef.Type != "int" {
		t.Errorf("n.Type = %q; want int", nDef.Type)
	}
	if nDef.Required {
		t.Errorf("n.Required = true; want false")
	}
}

// TestBuildCohereRequest_ToolResultsLifted verifies that a final TOOL turn
// is lifted into Cohere's request-level tool_results slot.
func TestBuildCohereRequest_ToolResultsLifted(t *testing.T) {
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "search for X"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				Name: "search",
				Args: map[string]any{"q": "X"},
			},
		}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				Name:     "search",
				Response: map[string]any{"output": "X result"},
			},
		}}},
	}
	req := buildCohereRequest("", contents, nil, false)
	// The final TOOL turn should be lifted out of chat_history into
	// tool_results.
	if len(req.ToolResults) != 1 {
		t.Fatalf("ToolResults len = %d; want 1", len(req.ToolResults))
	}
	if req.ToolResults[0].Call.Name != "search" {
		t.Errorf("ToolResults[0].Call.Name = %q; want search", req.ToolResults[0].Call.Name)
	}
	if req.ToolResults[0].Outputs[0].Output != "X result" {
		t.Errorf("Outputs[0].Output = %q; want \"X result\"",
			req.ToolResults[0].Outputs[0].Output)
	}
	// Chat history should now have just the user + CHATBOT turn.
	if len(req.ChatHistory) != 2 {
		t.Errorf("ChatHistory len = %d; want 2; got %+v",
			len(req.ChatHistory), req.ChatHistory)
	}
}
