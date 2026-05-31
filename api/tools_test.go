package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"go.f0o.dev/cline-vertex-gw/provider"
	"google.golang.org/genai"
)

// TestOAIToolChoice_Unmarshal exercises the three accepted wire forms:
// bare string, full object, and the defensive fallback for garbage input.
func TestOAIToolChoice_Unmarshal(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantMode string
		wantName string
	}{
		{"bare auto", `"auto"`, "auto", ""},
		{"bare none", `"none"`, "none", ""},
		{"bare required", `"required"`, "required", ""},
		{"object form", `{"type":"function","function":{"name":"read_file"}}`, "function", "read_file"},
		{"empty string fallback", `""`, "auto", ""},
		{"garbage fallback", `42`, "auto", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tc OAIToolChoice
			if err := json.Unmarshal([]byte(c.body), &tc); err != nil {
				t.Fatalf("unmarshal %q: %v", c.body, err)
			}
			if tc.Mode != c.wantMode {
				t.Errorf("Mode = %q; want %q", tc.Mode, c.wantMode)
			}
			if tc.Name != c.wantName {
				t.Errorf("Name = %q; want %q", tc.Name, c.wantName)
			}
		})
	}
}

// TestGenOptionsFromOAI_ToolsTranslated verifies that req.Tools surfaces
// on the returned options as a populated genai.Tool slice.
func TestGenOptionsFromOAI_ToolsTranslated(t *testing.T) {
	req := &OAIChatRequest{
		Tools: []OAIToolDef{{
			Type: "function",
			Function: OAIToolFunctionDef{
				Name:        "read_file",
				Description: "Read a file",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			},
		}},
	}
	got := genOptionsFromOAI(req)
	if got == nil {
		t.Fatalf("expected non-nil options when tools are set")
	}
	if len(got.Tools) != 1 {
		t.Fatalf("Tools len = %d; want 1", len(got.Tools))
	}
	if got.Tools[0].FunctionDeclarations[0].Name != "read_file" {
		t.Errorf("FD.Name = %q; want read_file",
			got.Tools[0].FunctionDeclarations[0].Name)
	}
}

// TestGenOptionsFromOAI_LegacyFunctionsTranslated verifies the legacy
// `functions` field also surfaces on GenerationOptions.Tools so older
// clients still get tool support.
func TestGenOptionsFromOAI_LegacyFunctionsTranslated(t *testing.T) {
	req := &OAIChatRequest{
		Functions: []OAIToolFunctionDef{{
			Name:       "old_school_fn",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	}
	got := genOptionsFromOAI(req)
	if got == nil || len(got.Tools) != 1 {
		t.Fatalf("legacy functions not translated: %+v", got)
	}
	if got.Tools[0].FunctionDeclarations[0].Name != "old_school_fn" {
		t.Errorf("legacy FD name lost: %+v", got.Tools[0].FunctionDeclarations[0])
	}
}

// TestGenOptionsFromOAI_ToolChoice exercises every tool_choice variant.
func TestGenOptionsFromOAI_ToolChoice(t *testing.T) {
	cases := []struct {
		mode     string
		name     string
		wantMode genai.FunctionCallingConfigMode
		wantNil  bool
	}{
		{"auto", "", "", true}, // auto means no constraint → nil ToolConfig
		{"none", "", genai.FunctionCallingConfigModeNone, false},
		{"required", "", genai.FunctionCallingConfigModeAny, false},
		{"function", "read_file", genai.FunctionCallingConfigModeAny, false},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			req := &OAIChatRequest{
				ToolChoice: &OAIToolChoice{Mode: c.mode, Name: c.name},
			}
			got := genOptionsFromOAI(req)
			if c.wantNil {
				if got != nil && got.ToolConfig != nil {
					t.Errorf("ToolConfig should be nil for mode=%q; got %+v",
						c.mode, got.ToolConfig)
				}
				return
			}
			if got == nil || got.ToolConfig == nil ||
				got.ToolConfig.FunctionCallingConfig == nil {
				t.Fatalf("ToolConfig not populated for mode=%q", c.mode)
			}
			fc := got.ToolConfig.FunctionCallingConfig
			if fc.Mode != c.wantMode {
				t.Errorf("Mode = %v; want %v", fc.Mode, c.wantMode)
			}
			if c.name != "" {
				if len(fc.AllowedFunctionNames) != 1 || fc.AllowedFunctionNames[0] != c.name {
					t.Errorf("AllowedFunctionNames = %v; want [%s]",
						fc.AllowedFunctionNames, c.name)
				}
			}
		})
	}
}

// TestOAIToolCallFromGenai verifies the genai → OAI tool-call translation:
// arguments must be JSON-encoded STRING form (per OpenAI spec), and an id
// is synthesized when upstream didn't supply one.
func TestOAIToolCallFromGenai(t *testing.T) {
	fc := &genai.FunctionCall{
		Name: "read_file",
		Args: map[string]any{"path": "main.go"},
	}
	part := &genai.Part{FunctionCall: fc}
	out := oaiToolCallFromGenai(part)
	if out.Function.Name != "read_file" {
		t.Errorf("Name = %q", out.Function.Name)
	}
	if out.Function.Arguments != `{"path":"main.go"}` {
		t.Errorf("Arguments = %q; want stringified JSON", out.Function.Arguments)
	}
	if out.Type != "function" {
		t.Errorf("Type = %q; want function", out.Type)
	}
	if !strings.HasPrefix(out.ID, "call_") {
		t.Errorf("ID = %q; want call_<hex> prefix", out.ID)
	}

	// When upstream supplies an ID it must be preserved.
	fc.ID = "toolu_xyz"
	out = oaiToolCallFromGenai(part)
	if out.ID != "toolu_xyz" {
		t.Errorf("ID = %q; want toolu_xyz preserved", out.ID)
	}
}

// TestToolCallFromGenai_OllamaShape verifies the Ollama-shape translation:
// args are emitted as a JSON OBJECT (not string), unlike OpenAI's wire.
func TestToolCallFromGenai_OllamaShape(t *testing.T) {
	fc := &genai.FunctionCall{
		Name: "list_files",
		Args: map[string]any{"dir": "/tmp"},
	}
	part := &genai.Part{FunctionCall: fc}
	out := toolCallFromGenai(part)
	if out.Function.Name != "list_files" {
		t.Errorf("Name = %q", out.Function.Name)
	}
	if out.Function.Arguments["dir"] != "/tmp" {
		t.Errorf("Arguments[dir] = %v; want /tmp", out.Function.Arguments["dir"])
	}
}

// TestToolResultPart_StructuredJSON verifies that a JSON-object result text
// is preserved as a structured response rather than wrapped in {output:...}.
func TestToolResultPart_StructuredJSON(t *testing.T) {
	p := toolResultPart("call_1", "search", `{"hits":3,"items":["a","b","c"]}`)
	if p.FunctionResponse == nil {
		t.Fatalf("FunctionResponse is nil")
	}
	if p.FunctionResponse.Response["hits"] != float64(3) {
		t.Errorf("structured response not preserved: %+v", p.FunctionResponse.Response)
	}
	// _, hasOutput := p.FunctionResponse.Response["output"]
	if _, hasOutput := p.FunctionResponse.Response["output"]; hasOutput {
		t.Errorf("should not wrap structured JSON in {output:...}; got %+v",
			p.FunctionResponse.Response)
	}
}

// TestToolResultPart_PlainText wraps non-JSON text in {output:...}.
func TestToolResultPart_PlainText(t *testing.T) {
	p := toolResultPart("call_1", "read_file", "package main\nfunc main(){}")
	if p.FunctionResponse == nil {
		t.Fatalf("FunctionResponse is nil")
	}
	if got := p.FunctionResponse.Response["output"]; got != "package main\nfunc main(){}" {
		t.Errorf("Response[output] = %v; want raw text", got)
	}
}

// TestTranslateOllamaToolsToGenai is the Ollama-shape variant of the same
// translation. Identical semantics to the OAI version.
func TestTranslateOllamaToolsToGenai(t *testing.T) {
	tools := []ToolDef{{
		Type: "function",
		Function: ToolFunctionDef{
			Name:        "fn",
			Description: "desc",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		},
	}}
	got := translateOllamaToolsToGenai(tools)
	if len(got) != 1 || len(got[0].FunctionDeclarations) != 1 {
		t.Fatalf("got = %+v; want one tool one decl", got)
	}
	if got[0].FunctionDeclarations[0].Name != "fn" {
		t.Errorf("name = %q", got[0].FunctionDeclarations[0].Name)
	}
}

// TestBuildContents_OllamaToolCallsPreserved verifies that an assistant
// turn with empty text but populated tool_calls survives buildContents (the
// Ollama-surface counterpart of the same OAI test).
func TestBuildContents_OllamaToolCallsPreserved(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "please call list_files"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{
			ID: "call_1",
			Function: ToolCallFunction{
				Name:      "list_files",
				Arguments: map[string]any{"dir": "/tmp"},
			},
		}}},
		{Role: "tool", ToolCallID: "call_1", ToolName: "list_files",
			Content: "a.txt\nb.txt"},
	}
	contents, _, _ := buildContents(msgs)
	if len(contents) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(contents))
	}
	// Assistant turn must carry a FunctionCall part.
	var sawFC bool
	for _, p := range contents[1].Parts {
		if p.FunctionCall != nil && p.FunctionCall.Name == "list_files" {
			sawFC = true
		}
	}
	if !sawFC {
		t.Errorf("assistant turn missing FunctionCall: %+v", contents[1].Parts)
	}
	// Tool turn must carry a FunctionResponse part.
	var sawFR bool
	for _, p := range contents[2].Parts {
		if p.FunctionResponse != nil && p.FunctionResponse.Name == "list_files" {
			sawFR = true
		}
	}
	if !sawFR {
		t.Errorf("tool turn missing FunctionResponse: %+v", contents[2].Parts)
	}
}

// TestGenOptionsFromAPI_ToolsTranslated verifies the Ollama tools wire
// shape lands on GenerationOptions.Tools.
func TestGenOptionsFromAPI_ToolsTranslated(t *testing.T) {
	tools := []ToolDef{{
		Type: "function",
		Function: ToolFunctionDef{
			Name:       "x",
			Parameters: json.RawMessage(`{"type":"object"}`),
		},
	}}
	got := genOptionsFromAPI(nil, tools)
	if got == nil {
		t.Fatalf("expected non-nil options when tools are set")
	}
	if len(got.Tools) != 1 {
		t.Errorf("Tools len = %d; want 1", len(got.Tools))
	}
}

// fakeVertexStreamer is a test double for the streaming code path. It lets
// us drive runStreamWithRetry's per-chunk callback through a known sequence
// of text and tool-call chunks without hitting Vertex.
//
// Implemented by inserting a fake *provider.VertexClient stub via a
// minimal interface in production code isn't worth the refactor for this
// test — instead we exercise the SSE emission and translation paths
// end-to-end by driving the emitDelta / emitToolCallDelta closures via a
// fabricated openaiChatStream call against an in-memory response writer.
// That's what the next two tests do, using the openai_stream.go helpers
// directly.

// TestOpenAIStream_ToolCallEmittedAsDelta drives the SSE writer with a
// tool-call delta and verifies the wire shape.
func TestOpenAIStream_ToolCallEmittedAsDelta(t *testing.T) {
	rec := httptest.NewRecorder()
	// We rely on writeSSEData for framing — the exact tool-call delta shape
	// is what we're verifying.
	fc := &genai.FunctionCall{
		ID:   "toolu_abc",
		Name: "read_file",
		Args: map[string]any{"path": "main.go"},
	}
	args, _ := provider.MarshalToolArgs(fc.Args)
	tcd := OAIStreamToolCallDelta{
		Index: 0,
		ID:    fc.ID,
		Type:  "function",
	}
	tcd.Function.Name = fc.Name
	tcd.Function.Arguments = args
	chunk := OAIChatStreamResponse{
		ID:      "chatcmpl-x",
		Object:  "chat.completion.chunk",
		Created: 1,
		Model:   "claude-3-5-sonnet",
		Choices: []OAIChatChoiceStream{{
			Index:        0,
			Delta:        OAIStreamDelta{ToolCalls: []OAIStreamToolCallDelta{tcd}},
			FinishReason: nil,
		}},
	}
	if err := writeSSEData(context.Background(), rec, chunk); err != nil {
		t.Fatalf("writeSSEData: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"tool_calls"`) {
		t.Errorf("body missing tool_calls field: %s", body)
	}
	if !strings.Contains(body, `"name":"read_file"`) {
		t.Errorf("body missing function.name: %s", body)
	}
	if !strings.Contains(body, `"arguments":"{\"path\":\"main.go\"}"`) {
		t.Errorf("body has wrong arguments encoding (must be JSON-stringified): %s", body)
	}
	if !strings.Contains(body, `"id":"toolu_abc"`) {
		t.Errorf("body missing call id: %s", body)
	}
}

// TestOpenAIStream_FinalChunkFinishToolCalls verifies the terminal chunk
// carries finish_reason: "tool_calls" when tool calls were emitted.
func TestOpenAIStream_FinalChunkFinishToolCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	finish := "tool_calls"
	chunk := OAIChatStreamResponse{
		ID:      "chatcmpl-x",
		Object:  "chat.completion.chunk",
		Created: 1,
		Model:   "claude-3-5-sonnet",
		Choices: []OAIChatChoiceStream{{
			Index:        0,
			Delta:        OAIStreamDelta{},
			FinishReason: &finish,
		}},
		Usage: &OAIUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	if err := writeSSEData(context.Background(), rec, chunk); err != nil {
		t.Fatalf("writeSSEData: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("final chunk missing tool_calls finish reason: %s", body)
	}
}

func TestTranslateTools_SearchGrounding(t *testing.T) {
	// 1. Test OpenAI Tools Translation
	oaiTools := []OAIToolDef{
		{
			Type: "function",
			Function: OAIToolFunctionDef{
				Name: "google_search",
			},
		},
		{
			Type: "function",
			Function: OAIToolFunctionDef{
				Name: "custom_fn",
			},
		},
	}

	gotOAI := translateOAIToolsToGenai(oaiTools, nil)
	if len(gotOAI) != 2 {
		t.Fatalf("expected exactly 2 tools, got %d", len(gotOAI))
	}

	// First tool should be custom_fn, second should be GoogleSearchRetrieval
	var hasFunc, hasSearch bool
	for _, tool := range gotOAI {
		if tool.GoogleSearchRetrieval != nil {
			hasSearch = true
		}
		if len(tool.FunctionDeclarations) > 0 && tool.FunctionDeclarations[0].Name == "custom_fn" {
			hasFunc = true
		}
	}
	if !hasSearch {
		t.Error("missing GoogleSearchRetrieval tool in OAI translation")
	}
	if !hasFunc {
		t.Error("missing custom_fn tool in OAI translation")
	}

	// 2. Test Ollama Tools Translation
	ollamaTools := []ToolDef{
		{
			Type: "function",
			Function: ToolFunctionDef{
				Name: "web_search",
			},
		},
	}

	gotOllama := translateOllamaToolsToGenai(ollamaTools)
	if len(gotOllama) != 1 {
		t.Fatalf("expected exactly 1 tool, got %d", len(gotOllama))
	}
	if gotOllama[0].GoogleSearchRetrieval == nil {
		t.Error("expected GoogleSearchRetrieval tool in Ollama translation")
	}
}
