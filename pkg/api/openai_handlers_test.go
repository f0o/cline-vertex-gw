package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestContentString_StringForm(t *testing.T) {
	m := OAIChatMessage{Role: "user", Content: json.RawMessage(`"hello world"`)}
	if got := m.ContentString(); got != "hello world" {
		t.Errorf("ContentString = %q; want %q", got, "hello world")
	}
}

func TestContentString_PartsForm(t *testing.T) {
	m := OAIChatMessage{
		Role: "user",
		Content: json.RawMessage(`[
			{"type":"text","text":"hello "},
			{"type":"text","text":"world"}
		]`),
	}
	if got := m.ContentString(); got != "hello world" {
		t.Errorf("ContentString = %q; want %q", got, "hello world")
	}
}

func TestContentString_PartsForm_SkipsUnknownTypes(t *testing.T) {
	m := OAIChatMessage{
		Role: "user",
		Content: json.RawMessage(`[
			{"type":"text","text":"hi"},
			{"type":"image_url","image_url":{"url":"data:..."}},
			{"type":"text","text":"!"}
		]`),
	}
	if got := m.ContentString(); got != "hi!" {
		t.Errorf("ContentString = %q; want %q", got, "hi!")
	}
}

func TestContentString_Empty(t *testing.T) {
	var m OAIChatMessage
	if got := m.ContentString(); got != "" {
		t.Errorf("ContentString of empty content = %q; want ''", got)
	}
}

func TestContentString_PartsForm_DefaultedType(t *testing.T) {
	// Some clients send `{"text":"..."}` without an explicit "type" field.
	m := OAIChatMessage{
		Role:    "user",
		Content: json.RawMessage(`[{"text":"bare"}]`),
	}
	if got := m.ContentString(); got != "bare" {
		t.Errorf("ContentString = %q; want %q", got, "bare")
	}
}

func TestBuildContentsOAI_SystemAndDeveloperHoisted(t *testing.T) {
	msgs := []OAIChatMessage{
		{Role: "system", Content: json.RawMessage(`"be terse"`)},
		{Role: "developer", Content: json.RawMessage(`"and helpful"`)},
		{Role: "user", Content: json.RawMessage(`"hi"`)},
	}
	contents, sys, _ := buildContentsOAI(msgs)
	if !strings.Contains(sys, "be terse") || !strings.Contains(sys, "and helpful") {
		t.Errorf("systemPrompt missing expected text: %q", sys)
	}
	if len(contents) != 1 {
		t.Fatalf("expected 1 content turn, got %d", len(contents))
	}
	if contents[0].Role == "" {
		t.Errorf("expected non-empty role on user turn, got empty")
	}
}

func TestBuildContentsOAI_MergesConsecutiveSameRole(t *testing.T) {
	msgs := []OAIChatMessage{
		{Role: "user", Content: json.RawMessage(`"a"`)},
		{Role: "user", Content: json.RawMessage(`"b"`)},
		{Role: "assistant", Content: json.RawMessage(`"c"`)},
		{Role: "user", Content: json.RawMessage(`"d"`)},
	}
	contents, _, _ := buildContentsOAI(msgs)
	if len(contents) != 3 {
		t.Fatalf("expected 3 turns (user,assistant,user), got %d", len(contents))
	}
	if len(contents[0].Parts) != 2 {
		t.Errorf("first turn parts = %d; want 2 (merged)", len(contents[0].Parts))
	}
}

func TestBuildContentsOAI_EmptyContentSkipped(t *testing.T) {
	msgs := []OAIChatMessage{
		{Role: "user", Content: json.RawMessage(`""`)},
		{Role: "user", Content: json.RawMessage(`"hello"`)},
	}
	contents, _, _ := buildContentsOAI(msgs)
	if len(contents) != 1 {
		t.Fatalf("expected 1 turn (empty skipped), got %d", len(contents))
	}
	if got := contents[0].Parts[0].Text; got != "hello" {
		t.Errorf("text = %q; want hello", got)
	}
}

func TestBuildContentsOAI_ToolRoleBecomesFunctionResponse(t *testing.T) {
	// A role:"tool" message must surface as a FunctionResponse part on a
	// user-role turn — not as a "[tool:X] ..." prose prefix. The previous
	// implementation folded the result into prose which broke tool-calling
	// continuity with every publisher that has a structured tool_result slot.
	msgs := []OAIChatMessage{
		{Role: "user", Content: json.RawMessage(`"call X"`)},
		{Role: "tool", Name: "X", ToolCallID: "call_abc",
			Content: json.RawMessage(`"the result"`)},
		{Role: "assistant", Content: json.RawMessage(`"ok"`)},
	}
	contents, _, _ := buildContentsOAI(msgs)
	// user (with text + tool_result) merges into a single user turn, then assistant.
	if len(contents) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(contents))
	}
	// The user turn should carry the tool result as a FunctionResponse part.
	var sawFR bool
	for _, p := range contents[0].Parts {
		if p.FunctionResponse != nil {
			sawFR = true
			if p.FunctionResponse.Name != "X" {
				t.Errorf("FunctionResponse.Name = %q; want X", p.FunctionResponse.Name)
			}
			if p.FunctionResponse.ID != "call_abc" {
				t.Errorf("FunctionResponse.ID = %q; want call_abc", p.FunctionResponse.ID)
			}
			if got, _ := p.FunctionResponse.Response["output"].(string); got != "the result" {
				t.Errorf("FunctionResponse.Response[output] = %v; want 'the result'", p.FunctionResponse.Response["output"])
			}
		}
	}
	if !sawFR {
		t.Errorf("expected a FunctionResponse part on the user turn; got parts: %+v", contents[0].Parts)
	}
}

func TestBuildContentsOAI_AssistantToolCallsPreserved(t *testing.T) {
	// An assistant turn whose only payload is tool_calls (no text content)
	// must NOT be skipped — that was the bug that broke multi-turn tool
	// conversations on /v1/chat/completions.
	msgs := []OAIChatMessage{
		{Role: "user", Content: json.RawMessage(`"please call read_file"`)},
		{Role: "assistant", Content: json.RawMessage(`""`), ToolCalls: []OAIToolCall{
			{
				ID:   "call_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "read_file", Arguments: `{"path":"main.go"}`},
			},
		}},
		{Role: "tool", Name: "read_file", ToolCallID: "call_1",
			Content: json.RawMessage(`"package main..."`)},
	}
	contents, _, _ := buildContentsOAI(msgs)
	// user + assistant(tool_call) + user(tool_result) → 3 turns.
	if len(contents) != 3 {
		t.Fatalf("expected 3 turns, got %d; contents=%+v", len(contents), contents)
	}
	// Second turn must be the assistant turn with a FunctionCall part.
	if contents[1].Role != "model" {
		t.Errorf("contents[1].Role = %q; want model", contents[1].Role)
	}
	var sawFC bool
	for _, p := range contents[1].Parts {
		if p.FunctionCall != nil {
			sawFC = true
			if p.FunctionCall.Name != "read_file" {
				t.Errorf("FunctionCall.Name = %q; want read_file", p.FunctionCall.Name)
			}
			if p.FunctionCall.ID != "call_1" {
				t.Errorf("FunctionCall.ID = %q; want call_1", p.FunctionCall.ID)
			}
			if got, _ := p.FunctionCall.Args["path"].(string); got != "main.go" {
				t.Errorf("FunctionCall.Args[path] = %v; want main.go", p.FunctionCall.Args["path"])
			}
		}
	}
	if !sawFC {
		t.Errorf("expected a FunctionCall part on the assistant turn; got parts: %+v", contents[1].Parts)
	}
}

func TestGenOptionsFromOAI_AllUnset(t *testing.T) {
	req := &OAIChatRequest{}
	if got := genOptionsFromOAI(req); got != nil {
		t.Errorf("expected nil for all-unset request, got %+v", got)
	}
}

func TestGenOptionsFromOAI_MaxTokensAlias(t *testing.T) {
	var v int32 = 256
	req := &OAIChatRequest{MaxOutputTokens: &v}
	got := genOptionsFromOAI(req)
	if got == nil || got.MaxTokens == nil || *got.MaxTokens != 256 {
		t.Errorf("MaxOutputTokens alias not honored: %+v", got)
	}
}

func TestGenOptionsFromOAI_MaxTokensPrefersExplicit(t *testing.T) {
	var primary int32 = 128
	var alias int32 = 999
	req := &OAIChatRequest{MaxTokens: &primary, MaxOutputTokens: &alias}
	got := genOptionsFromOAI(req)
	if got == nil || got.MaxTokens == nil || *got.MaxTokens != 128 {
		t.Errorf("MaxTokens did not take precedence over alias: %+v", got)
	}
}

func TestGenOptionsFromOAI_Stop(t *testing.T) {
	req := &OAIChatRequest{Stop: []string{"</done>"}}
	got := genOptionsFromOAI(req)
	if got == nil || !reflect.DeepEqual(got.Stop, []string{"</done>"}) {
		t.Errorf("Stop not propagated: %+v", got)
	}
}

func TestFinishReasonOAI(t *testing.T) {
	cases := map[string]string{
		"":                         "stop",
		"STOP":                     "stop",
		"FINISH_REASON_STOP":       "stop",
		"MAX_TOKENS":               "length",
		"FINISH_REASON_MAX_TOKENS": "length",
		"SAFETY":                   "content_filter",
		"FINISH_REASON_SAFETY":     "content_filter",
		"RECITATION":               "content_filter",
		"FINISH_REASON_RECITATION": "content_filter",
		"TOOL_USE":                 "tool_calls",
		"TOOL_CALLS":               "tool_calls",
		"some-other-thing":         "stop",
	}
	for in, want := range cases {
		if got := finishReasonOAI(in); got != want {
			t.Errorf("finishReasonOAI(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestHTTPStatusForUpstreamError(t *testing.T) {
	// We can't easily construct typed errors that re-classify, but we can
	// exercise the default branch and the obvious classifier strings.
	cases := []struct {
		err        error
		wantStatus int
	}{
		{nil, http.StatusBadGateway}, // classifyError(nil)=="ok" -> default
	}
	for _, c := range cases {
		gotStatus, gotType := httpStatusForUpstreamError(c.err)
		if gotStatus != c.wantStatus {
			t.Errorf("status for %v = %d; want %d", c.err, gotStatus, c.wantStatus)
		}
		if gotType == "" {
			t.Errorf("type for %v is empty", c.err)
		}
	}
}

func TestWriteOAIError_Shape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOAIError(rec, http.StatusBadRequest, "invalid_request_error", "bad", "field_x")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q; want application/json", ct)
	}
	var got OAIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if got.Error.Message != "bad" || got.Error.Type != "invalid_request_error" || got.Error.Code != "field_x" {
		t.Errorf("error envelope mismatch: %+v", got.Error)
	}
}

func TestOpenAIChatCompletions_RejectsGET(t *testing.T) {
	h := &APIHandler{}
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.OpenAIChatCompletionsHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", rec.Code)
	}
}

func TestOpenAIChatCompletions_NoVertexClient(t *testing.T) {
	// When the Vertex client is unconfigured the handler must return a
	// well-formed OpenAI error envelope rather than panic.
	h := &APIHandler{Vertex: nil}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.OpenAIChatCompletionsHandler(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
	var got OAIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not an OAI error envelope: %v body=%s", err, rec.Body.String())
	}
	if got.Error.Message == "" {
		t.Errorf("expected non-empty error message, got %+v", got.Error)
	}
}

func TestOpenAIModels_RejectsPOST(t *testing.T) {
	h := &APIHandler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.OpenAIModelsHandler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", rec.Code)
	}
	var got OAIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not an OAI error envelope: %v", err)
	}
}

func TestOpenAIModels_NoVertexClient(t *testing.T) {
	h := &APIHandler{Vertex: nil}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.OpenAIModelsHandler(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestAuthMiddleware_V1PathProtected(t *testing.T) {
	h := AuthMiddleware("secret", dummyHandler)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 (missing token on /v1/*)", rec.Code)
	}
}

func TestAuthMiddleware_V1PathCorrectToken(t *testing.T) {
	h := AuthMiddleware("secret", dummyHandler)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 with valid token on /v1/*", rec.Code)
	}
}

func TestIsProtectedPath(t *testing.T) {
	cases := map[string]bool{
		"/":                    false,
		"/api/tags":            true,
		"/api/chat":            true,
		"/api/anything":        true,
		"/v1/models":           true,
		"/v1/chat/completions": true,
		"/v1":                  false, // no trailing slash
		"/api":                 false, // no trailing slash
		"/healthz":             false,
		"/v1foo":               false, // not "/v1/"
	}
	for p, want := range cases {
		if got := isProtectedPath(p); got != want {
			t.Errorf("isProtectedPath(%q) = %v; want %v", p, got, want)
		}
	}
}

func TestWriteSSEData_Framing(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := writeSSEData(context.Background(), rec, map[string]any{"k": "v"}); err != nil {
		t.Fatalf("writeSSEData: %v", err)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Errorf("missing SSE prefix: %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("missing SSE terminator: %q", body)
	}
	// The middle must be valid JSON.
	jsonPart := strings.TrimSuffix(strings.TrimPrefix(body, "data: "), "\n\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(jsonPart), &m); err != nil {
		t.Errorf("payload not valid JSON: %v (%q)", err, jsonPart)
	}
}

func TestOAIChatStreamResponse_FinishReasonNullByDefault(t *testing.T) {
	// Mid-stream chunks must serialize finish_reason as null (not omitted)
	// so OpenAI clients can rely on its presence.
	chunk := OAIChatStreamResponse{
		ID:      "x",
		Object:  "chat.completion.chunk",
		Created: 1,
		Model:   "m",
		Choices: []OAIChatChoiceStream{{
			Index:        0,
			Delta:        OAIStreamDelta{Content: "hi"},
			FinishReason: nil,
		}},
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"finish_reason":null`) {
		t.Errorf("expected finish_reason:null in mid-stream chunk, got %s", out)
	}
}
