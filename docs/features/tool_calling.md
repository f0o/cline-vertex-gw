# Tool Calling (Function Calling)

`cline-vertex-gw` fully bridges the gap between client tool-calling semantics and Vertex AI upstreams. 

When a client request includes a `tools` array (or the legacy `functions` field on `/v1/chat/completions`), the gateway translates it into the upstream's native shape (e.g. Anthropic tools, OpenAI-compatible tools, Cohere tools, or Gemini Function Declarations). When the model emits a tool call, the gateway translates it back into the client's expected wire format.

---

## Tool Translation Mapping

### Inbound (Client → Gateway)

| Client Feature | Translated Upstream Shape | Notes |
|---|---|---|
| `tools: [...]` array | Anthropic `tools`, OpenAI-compat `tools`, Cohere `tools`, Gemini `FunctionDeclaration` | Cohere's tool schema expects a flat list of `parameter_definitions`, which the gateway automatically flattens from standard JSON Schema. |
| `tool_choice` parameter | Anthropic `tool_choice`, OpenAI `tool_choice`, Gemini `FunctionCallingConfig`, Cohere tools presence/absence | Supports standard tool choices: `"auto"`, `"none"`, `"required"`, or a specific function object `{type: "function", function: {name: "..."}}`. |
| Assistant `tool_calls` (prior turn) | Anthropic `tool_use` blocks, OpenAI assistant `tool_calls`, Cohere `CHATBOT` tool calls, Gemini `FunctionCall` part | Preserves conversation context across turns when agent-loop history is submitted. |
| `role: "tool"` response | Anthropic `tool_result` block, OpenAI `tool` message, Cohere `tool_results` (lifted to request level), Gemini `FunctionResponse` part | Translates tool outputs back to the model's preferred input format. |

### Outbound (Gateway → Client)

| Context | Wire Shape | Notes |
|---|---|---|
| Streaming on `/v1/chat/completions` | `delta.tool_calls[]` | Emits `id`, `function.name`, and `function.arguments` as a JSON-encoded string delta-by-delta per standard OpenAI spec. Tool calls are fully assembled chunk-by-chunk. |
| Non-streaming on `/v1/chat/completions` | `choices[0].message.tool_calls[]` | Returns fully formed tool call objects. |
| Streaming/Non-streaming on `/api/chat` (Ollama) | `message.tool_calls[]` | Emitted on the terminal `Done` frame, with tool `arguments` presented as a JSON object (standard Ollama convention, **not** a stringified JSON block). |
| `finish_reason` handling | `"tool_calls"` (OpenAI) or `"tool_use"` (Ollama) | Gateway defensively upgrades the client-visible finish reason even when an upstream publisher incorrectly reports `"stop"` despite emitting a tool use. |

---

## Smoke Test Example

Verify tool calling using `/v1/chat/completions`:

```bash
curl -sS http://127.0.0.1:11434/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet",
    "stream": false,
    "tools": [{
      "type": "function",
      "function": {
        "name": "get_weather",
        "description": "Get current weather for a location",
        "parameters": {
          "type": "object",
          "properties": {
            "location": {"type": "string"}
          },
          "required": ["location"]
        }
      }
    }],
    "messages": [{"role": "user", "content": "Weather in Paris?"}]
  }'
```

*Expected response containing translated tool calls:*
```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "created": 1747737600,
  "model": "claude-3-5-sonnet",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": null,
        "tool_calls": [
          {
            "id": "toolu_...",
            "type": "function",
            "function": {
              "name": "get_weather",
              "arguments": "{\"location\":\"Paris\"}"
            }
          }
        ]
      },
      "finish_reason": "tool_calls"
    }
  ]
}
```
