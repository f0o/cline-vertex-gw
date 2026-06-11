package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

func TestMCPServer_Initialize(t *testing.T) {
	// Prepare input mock stdin
	input := `{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}` + "\n"
	r, w := io.Pipe()

	// Redirect stdout to capture response
	oldStdout := osStdout
	pr, pw := io.Pipe()
	osStdout = pw

	// Run server in goroutine
	go func() {
		// Mock RunMCPServer manually using our helper to avoid block
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			var req RPCRequest
			_ = json.Unmarshal(scanner.Bytes(), &req)
			handleRequest(pw, &req)
		}
		pw.Close()
	}()

	// Write to stdin pipe
	_, _ = io.WriteString(w, input)
	w.Close()

	// Read and verify response
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, pr)
	osStdout = oldStdout

	var resp RPCResponse
	err := json.Unmarshal(buf.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to unmarshal JSON-RPC response: %v, raw: %s", err, buf.String())
	}

	if resp.JSONRPC != "2.0" {
		t.Errorf("expected JSON-RPC 2.0 version, got: %s", resp.JSONRPC)
	}

	if resp.ID.(float64) != 1 {
		t.Errorf("expected ID 1, got: %v", resp.ID)
	}

	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result to be a map, got: %T", resp.Result)
	}

	if resultMap["protocolVersion"] != "2024-11-05" {
		t.Errorf("expected protocolVersion 2024-11-05, got: %v", resultMap["protocolVersion"])
	}
}

func TestMCPServer_ToolsList(t *testing.T) {
	input := `{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}}` + "\n"
	r, w := io.Pipe()

	oldStdout := osStdout
	pr, pw := io.Pipe()
	osStdout = pw

	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			var req RPCRequest
			_ = json.Unmarshal(scanner.Bytes(), &req)
			handleRequest(pw, &req)
		}
		pw.Close()
	}()

	_, _ = io.WriteString(w, input)
	w.Close()

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, pr)
	osStdout = oldStdout

	var resp RPCResponse
	err := json.Unmarshal(buf.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to unmarshal JSON-RPC response: %v, raw: %s", err, buf.String())
	}

	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result to be a map, got: %T", resp.Result)
	}

	tools, ok := resultMap["tools"].([]any)
	if !ok {
		t.Fatalf("expected tools to be a slice, got: %T", resultMap["tools"])
	}

	if len(tools) != 2 {
		t.Errorf("expected 2 tools, got: %d", len(tools))
	}

	firstTool := tools[0].(map[string]any)
	if firstTool["name"] != "compress_prompt" {
		t.Errorf("expected first tool to be 'compress_prompt', got: %v", firstTool["name"])
	}
}

