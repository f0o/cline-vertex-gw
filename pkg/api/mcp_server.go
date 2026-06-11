package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/cache"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"go.f0o.dev/cline-vertex-gw/pkg/pipeline"
	"io"
	"os"
	"strings"

	"google.golang.org/genai"
)

var logMCP = logx.Scoped("mcp_server")

// RPCRequest represents a standard JSON-RPC 2.0 request.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCResponse represents a standard JSON-RPC 2.0 response.
type RPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

var osStdout io.Writer = os.Stdout

// RunMCPServer runs a standard MCP server loop on stdin/stdout.
func RunMCPServer() {
	logMCP.Infof("Starting Model Context Protocol (MCP) server over stdin/stdout...")

	// All logs MUST go to stderr to avoid corrupting stdout JSON-RPC stream
	scanner := bufio.NewScanner(os.Stdin)
	writer := osStdout

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req RPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			sendError(writer, nil, -32700, "Parse error: "+err.Error())
			continue
		}

		if req.JSONRPC != "2.0" {
			sendError(writer, req.ID, -32600, "Invalid Request: missing jsonrpc version")
			continue
		}

		handleRequest(writer, &req)
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		logMCP.Errorf("MCP stdin scanner error: %v", err)
	}
}

func handleRequest(w io.Writer, req *RPCRequest) {
	switch req.Method {
	case "initialize":
		res := map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "cline-vertex-gw-mcp",
				"version": "1.0.0",
			},
		}
		sendResult(w, req.ID, res)

	case "tools/list":
		tools := []map[string]any{
			{
				"name":        "compress_prompt",
				"description": "Aggressively compresses raw prompts, logs, or file pastes using cline-vertex-gw's advanced rule-based syntactic compression and smart log/JSON crushing.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"prompt": map[string]any{
							"type":        "string",
							"description": "The raw text, code block, or JSON body to compress.",
						},
					},
					"required": []string{"prompt"},
				},
			},
			{
				"name":        "retrieve_original",
				"description": "Retrieves the original, uncompacted content of a previously truncated/elided historical turn or tool response from the FSCache using its SHA-256 hash.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"hash": map[string]any{
							"type":        "string",
							"description": "The cryptographic hex SHA-256 hash from the elision placeholder (e.g. hash=f83a...)",
						},
					},
					"required": []string{"hash"},
				},
			},
		}
		sendResult(w, req.ID, map[string]any{"tools": tools})

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			sendError(w, req.ID, -32602, "Invalid params: "+err.Error())
			return
		}

		handleToolCall(w, req.ID, params.Name, params.Arguments)

	case "notifications/initialized":
		// No-op for MCP initialized notifications

	default:
		sendError(w, req.ID, -32601, "Method not found: "+req.Method)
	}
}

func handleToolCall(w io.Writer, id any, toolName string, args json.RawMessage) {
	switch toolName {
	case "compress_prompt":
		var arguments struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(args, &arguments); err != nil {
			sendError(w, id, -32602, "Invalid compress_prompt arguments: "+err.Error())
			return
		}

		// Apply the compression pipeline to the prompt
		var contents []*genai.Content
		if strings.Contains(arguments.Prompt, "```") {
			// Try syntactic code compression
			crushed, _ := pipeline.CompressCodeBlocks(arguments.Prompt)
			contents = append(contents, &genai.Content{
				Parts: []*genai.Part{{Text: crushed}},
			})
		} else {
			// Try general smart crushing (JSON/logs)
			crushed, _ := pipeline.SmartCrush(arguments.Prompt)
			contents = append(contents, &genai.Content{
				Parts: []*genai.Part{{Text: crushed}},
			})
		}

		compressedPrompt := contents[0].Parts[0].Text
		contentRes := []map[string]any{
			{
				"type": "text",
				"text": compressedPrompt,
			},
		}
		sendResult(w, id, map[string]any{"content": contentRes})

	case "retrieve_original":
		var arguments struct {
			Hash string `json:"hash"`
		}
		if err := json.Unmarshal(args, &arguments); err != nil {
			// Fallback if arguments is direct string
			var strArgs string
			if err2 := json.Unmarshal(args, &strArgs); err2 == nil {
				arguments.Hash = strArgs
			} else {
				// Try parsing map
				var mapArgs map[string]string
				if err3 := json.Unmarshal(args, &mapArgs); err3 == nil {
					arguments.Hash = mapArgs["hash"]
				}
			}
		}

		if arguments.Hash == "" {
			sendError(w, id, -32602, "Missing hash parameter")
			return
		}

		fileName := "elided_" + arguments.Hash + ".json"
		var originalString string
		_, err := cache.ReadFSCache(fileName, &originalString)
		if err != nil {
			sendError(w, id, -32000, fmt.Sprintf("Failed to read cached content for hash %s: %v", arguments.Hash, err))
			return
		}

		if originalString == "" {
			sendError(w, id, -32001, fmt.Sprintf("Hash %s not found in cache", arguments.Hash))
			return
		}

		contentRes := []map[string]any{
			{
				"type": "text",
				"text": originalString,
			},
		}
		sendResult(w, id, map[string]any{"content": contentRes})

	default:
		sendError(w, id, -32601, "Tool not found: "+toolName)
	}
}

func sendResult(w io.Writer, id any, result any) {
	res := RPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	send(w, res)
}

func sendError(w io.Writer, id any, code int, message string) {
	res := RPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: map[string]any{
			"code":    code,
			"message": message,
		},
	}
	send(w, res)
}

func send(w io.Writer, res RPCResponse) {
	data, err := json.Marshal(res)
	if err != nil {
		logMCP.Errorf("failed to marshal JSON-RPC response: %v", err)
		return
	}
	// Write response to writer with a trailing newline
	_, _ = w.Write(append(data, '\n'))
}
