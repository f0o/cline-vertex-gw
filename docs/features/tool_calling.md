# Tool Calling & Function Translation

`cline-vertex-gw` fully bridges the gap between client-side tool calling (function calling) semantics and the unique APIs of Vertex AI's upstream publishers.

When a client request registers a `tools` catalog (or the legacy `functions` array), the gateway automatically compiles the definitions into the exact wire format expected by the selected publisher (Anthropic, Cohere, OpenAI-compatible, or Gemini). When a model returns tool executions, the gateway intercepts and translates them back into standard client shapes on-the-fly.

---

## Tool Specification Translations

### Inbound Map (Client → Upstream)

| Client Feature | Upstream Mapping | Operational Behavior |
|---|---|---|
| `tools` array | `FunctionDeclaration` / `tools` schema | translates parameters, properties, and constraints. For **Cohere**, the nested JSON Schema is automatically flattened into Cohere's flat list of parameter definitions. |
| `tool_choice` | Upstream calling configs | Maps standard choices (`"auto"`, `"none"`, `"required"`, or a specific object `{type: "function", function: {name: "..."}}`) to the publisher's native configuration parameters (e.g. Gemini `FunctionCallingConfig` or Anthropic `tool_choice`). |
| Prior Assistant `tool_calls` | Native assistant blocks | Reconstructs and inserts function-call contexts (e.g., Anthropic `tool_use` blocks or Gemini `FunctionCall` parts) when the client submits conversation loops. |
| Prior User `tool` response | Native execution results | Translates the outputs of executed tools (e.g. `FunctionResponse` or `tool_result` blocks) back to the upstream model. |

### Outbound Map (Upstream → Client)

| Context | Client-Side Wire Shape | Translation Details |
|---|---|---|
| **Streaming (`/v1/*`)** | `delta.tool_calls[]` | Tool call arguments are streamed chunk-by-chunk and emitted delta-by-delta as JSON-encoded strings. The gateway tracks assembly internally to support seamless streaming. |
| **Non-Streaming (`/v1/*`)** | `choices[0].message.tool_calls[]` | Emits fully formed tool-call blocks in standard OpenAI-compatible formats. |
| **Ollama Surface (`/api/*`)** | `message.tool_calls[]` | Tool calls are fully assembled and flushed on the final **`Done` frame**. Arguments are sent as a structured JSON object (standard Ollama convention) rather than an escaped string. |
| **Finish Reason Upgrades** | `finish_reason: "tool_calls"` | Upstream models sometimes prematurely report a normal finish reason `"stop"` while submitting tool calls. The gateway dynamically corrects this to `"tool_calls"` or `"tool_use"` to prevent client execution halts. |

---

## Google Gemini 3.5 Thinking Models Alignment

The gateway includes specific alignments to support Gemini 3.5 thinking models:
- **Thought Signature Support:** Gemini 3.5 models enforce strict positional checks on thoughts and tool-use blocks during history replays. The gateway utilizes a local fallback `skip_thought_signature_validator` to safely reconstruct tool history turns without triggering bad request errors from the upstream API.
- **Part-by-Part Positional Alignment:** During conversation history reconstruction, `AlignFunctionCallsAndResponses` ensures tool calls and tool results are aligned exactly index-for-index with corresponding text placeholders.

---

## Smoke Test Example

You can smoke test tool calling against Claude or Gemini using `/v1/chat/completions`:

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
