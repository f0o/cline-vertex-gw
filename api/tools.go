package api

import (
	"encoding/json"
	"strings"

	"go.f0o.dev/cline-vertex-gw/provider"
	"google.golang.org/genai"
)

// Tool-related helpers shared by the OpenAI (/v1/*) and Ollama (/api/*)
// surfaces. The mapping in both directions uses genai.Tool /
// genai.FunctionDeclaration / genai.FunctionCall / genai.FunctionResponse as
// the in-process lingua franca — every publisher adapter speaks that shape.

// newToolCallID returns a stable, OpenAI-compatible synthetic id of the form
// "call_<hex>" for upstreams that didn't supply one. Anthropic always
// supplies an id; OpenAI-compat MaaS publishers usually do too; Cohere does
// not. Clients keying on the id field expect it to be present.
func newToolCallID() string {
	return "call_" + newReqID()
}

// toolCallFromGenai converts a model-emitted genai.FunctionCall into the
// Ollama-shape ToolCall used on /api/chat. Synthesizes an id if upstream
// didn't supply one.
func toolCallFromGenai(fc *genai.FunctionCall) ToolCall {
	if fc == nil {
		return ToolCall{}
	}
	id := fc.ID
	if id == "" {
		id = newToolCallID()
	}
	return ToolCall{
		ID: id,
		Function: ToolCallFunction{
			Name:      fc.Name,
			Arguments: fc.Args,
		},
	}
}

// oaiToolCallFromGenai converts a model-emitted genai.FunctionCall into the
// OpenAI-shape OAIToolCall used on /v1/chat/completions. Per OpenAI spec the
// arguments field is a JSON-encoded STRING.
func oaiToolCallFromGenai(fc *genai.FunctionCall) OAIToolCall {
	if fc == nil {
		return OAIToolCall{}
	}
	args, _ := provider.MarshalToolArgs(fc.Args)
	out := OAIToolCall{
		ID:   fc.ID,
		Type: "function",
	}
	if out.ID == "" {
		out.ID = newToolCallID()
	}
	out.Function.Name = fc.Name
	out.Function.Arguments = args
	return out
}

// translateOAIToolsToGenai converts the inbound OpenAI `tools` array into the
// internal genai.Tool representation. Legacy `functions` (the pre-2023
// OpenAI shape) is also accepted via translateOAIFunctionsToGenai and merged
// into the same return.
//
// Returns nil if no tool definitions were supplied. Callers should set the
// resulting slice on GenerationOptions.Tools.
func translateOAIToolsToGenai(tools []OAIToolDef, functions []OAIToolFunctionDef) []*genai.Tool {
	var fds []*genai.FunctionDeclaration
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue // we only support function tools
		}
		if t.Function.Name == "" {
			continue
		}
		fds = append(fds, oaiToolFunctionDefToGenai(t.Function))
	}
	for _, f := range functions {
		if f.Name == "" {
			continue
		}
		fds = append(fds, oaiToolFunctionDefToGenai(f))
	}
	if len(fds) == 0 {
		return nil
	}
	return []*genai.Tool{{FunctionDeclarations: fds}}
}

// oaiToolFunctionDefToGenai converts one OAI function definition into a
// genai.FunctionDeclaration. The Parameters JSON Schema is preserved
// verbatim via ParametersJsonSchema — every publisher adapter we ship
// handles that branch.
func oaiToolFunctionDefToGenai(f OAIToolFunctionDef) *genai.FunctionDeclaration {
	fd := &genai.FunctionDeclaration{
		Name:        f.Name,
		Description: f.Description,
	}
	if len(f.Parameters) > 0 {
		// Pass the raw JSON Schema through so publisher adapters can shape it
		// however their upstream needs (Anthropic: pass-through; OpenAI-compat:
		// pass-through; Cohere: extract flat param defs).
		fd.ParametersJsonSchema = f.Parameters
	}
	return fd
}

// translateOAIToolChoiceToGenai maps an OpenAI tool_choice value to the
// internal genai.ToolConfig representation. Returns nil for the default
// "auto" case so the publisher adapters can apply their own default.
func translateOAIToolChoiceToGenai(c *OAIToolChoice) *genai.ToolConfig {
	if c == nil || c.Mode == "" || c.Mode == "auto" {
		return nil
	}
	cfg := &genai.ToolConfig{
		FunctionCallingConfig: &genai.FunctionCallingConfig{},
	}
	switch c.Mode {
	case "none":
		cfg.FunctionCallingConfig.Mode = genai.FunctionCallingConfigModeNone
	case "required":
		cfg.FunctionCallingConfig.Mode = genai.FunctionCallingConfigModeAny
	case "function":
		cfg.FunctionCallingConfig.Mode = genai.FunctionCallingConfigModeAny
		cfg.FunctionCallingConfig.AllowedFunctionNames = []string{c.Name}
	default:
		return nil
	}
	return cfg
}

// translateOllamaToolsToGenai mirrors translateOAIToolsToGenai for the
// Ollama wire shape (which is structurally identical to OpenAI's `tools`).
func translateOllamaToolsToGenai(tools []ToolDef) []*genai.Tool {
	var fds []*genai.FunctionDeclaration
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		if t.Function.Name == "" {
			continue
		}
		fd := &genai.FunctionDeclaration{
			Name:        t.Function.Name,
			Description: t.Function.Description,
		}
		if len(t.Function.Parameters) > 0 {
			fd.ParametersJsonSchema = t.Function.Parameters
		}
		fds = append(fds, fd)
	}
	if len(fds) == 0 {
		return nil
	}
	return []*genai.Tool{{FunctionDeclarations: fds}}
}

// oaiToolCallToGenaiPart converts an inbound OAI assistant-replay tool_call
// into a genai.Part carrying a FunctionCall. Arguments is JSON-decoded from
// its wire-format string; malformed JSON yields Args=nil with no error.
func oaiToolCallToGenaiPart(tc OAIToolCall) *genai.Part {
	args, _ := provider.UnmarshalToolArgs(tc.Function.Arguments)
	return &genai.Part{
		FunctionCall: &genai.FunctionCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: args,
		},
	}
}

// ollamaToolCallToGenaiPart is the Ollama-shape counterpart. Arguments is
// already a map (Ollama doesn't stringify it).
func ollamaToolCallToGenaiPart(tc ToolCall) *genai.Part {
	return &genai.Part{
		FunctionCall: &genai.FunctionCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: tc.Function.Arguments,
		},
	}
}

// toolResultPart builds a genai.Part carrying a FunctionResponse for an
// inbound role:"tool" message. Both surfaces share this builder.
//
// The text payload is wrapped in a single-key map ({"output": "..."}) which
// is the conventional JSON-shape for tool results — every publisher adapter
// understands it via FunctionResponseToText, which prefers the "output" key.
//
// When the result text parses as a JSON object we use it directly as the
// response so structured tool results round-trip faithfully.
func toolResultPart(toolCallID, name, resultText string) *genai.Part {
	if name == "" {
		name = "tool"
	}
	var resp map[string]any
	trimmed := strings.TrimSpace(resultText)
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &resp); err == nil {
			return &genai.Part{FunctionResponse: &genai.FunctionResponse{
				ID:       toolCallID,
				Name:     name,
				Response: resp,
			}}
		}
	}
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		ID:       toolCallID,
		Name:     name,
		Response: map[string]any{"output": resultText},
	}}
}

