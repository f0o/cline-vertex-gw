# API Reference

The gateway hosts a dual-dialect API surface. Both interfaces are served on the same TCP port, routing transparently through standard route-pattern matching.

---

## 🗺️ comparative Endpoint Matrix

| Ollama Dialect Path | OpenAI Dialect Path | HTTP Method | Functionality | Auth Protected? |
| :--- | :--- | :--- | :--- | :--- |
| `GET /api/tags` | `GET /v1/models` | `GET` | Discovers and list all available model identities | Yes (if configured) |
| `POST /api/chat` | `POST /v1/chat/completions` | `POST` | Executes streaming and non-streaming conversation loops | Yes (if configured) |
| `POST /api/generate`| N/A | `POST` | Executes single-turn text completion prompts | Yes (if configured) |
| N/A | `GET /` | `GET` | Health check endpoint returning simple greeting | No |

---

## 🏷️ Model Discovery

The discovery layer queries all supported and enabled publishers in parallel to compile a unified model registry. Model IDs are prefixed with their publisher namespaces (e.g. `google/gemini-2.5-pro` or `anthropic/claude-3-5-sonnet-v2`).

### 1. Ollama Model Discovery (`GET /api/tags`)

Used by Ollama clients to fetch available models.

#### Request Example
```bash
curl http://localhost:11434/api/tags
```

#### Response Payload (JSON)
```json
{
  "models": [
    {
      "name": "google/gemini-2.5-pro",
      "model": "google/gemini-2.5-pro",
      "modified_at": "2026-06-11T07:44:00Z",
      "size": 0,
      "digest": "vertex-ai-digest",
      "details": {
        "format": "gguf",
        "family": "gemini",
        "families": ["gemini"],
        "parameter_size": "unknown",
        "quantization_level": "none"
      }
    },
    {
      "name": "anthropic/claude-3-5-sonnet-v2",
      "model": "anthropic/claude-3-5-sonnet-v2",
      "modified_at": "2026-06-11T07:44:00Z",
      "size": 0,
      "digest": "vertex-ai-digest",
      "details": {
        "format": "gguf",
        "family": "claude",
        "families": ["claude"],
        "parameter_size": "unknown",
        "quantization_level": "none"
      }
    }
  ]
}
```

### 2. OpenAI Model Discovery (`GET /v1/models`)

Exposes the same registry formatted as standard OpenAI resource definitions.

#### Request Example
```bash
curl -H "Authorization: Bearer your-token" http://localhost:11434/v1/models
```

#### Response Payload (JSON)
```json
{
  "object": "list",
  "data": [
    {
      "id": "google/gemini-2.5-pro",
      "object": "model",
      "created": 1781163840,
      "owned_by": "google"
    },
    {
      "id": "anthropic/claude-3-5-sonnet-v2",
      "object": "model",
      "created": 1781163840,
      "owned_by": "anthropic"
    }
  ]
}
```

---

## 💬 Chat Completions

Both streaming and non-streaming modes are fully supported. Streaming is heavily optimized and flushes chunks down the wire as soon as they are received.

### 1. Ollama Chat Completion (`POST /api/chat`)

Accepts and returns Ollama's Newline-Delimited JSON (NDJSON) streaming format.

#### Request Example
```bash
curl -X POST http://localhost:11434/api/chat -d '{
  "model": "google/gemini-2.5-pro",
  "messages": [
    {"role": "user", "content": "Tell me a joke."}
  ],
  "stream": true
}'
```

#### Stream Responses (NDJSON Chunks)
```json
{"model":"google/gemini-2.5-pro","created_at":"2026-06-11T07:44:00Z","message":{"role":"assistant","content":"Why "},"done":false}
{"model":"google/gemini-2.5-pro","created_at":"2026-06-11T07:44:00Z","message":{"role":"assistant","content":"did the "},"done":false}
{"model":"google/gemini-2.5-pro","created_at":"2026-06-11T07:44:00Z","message":{"role":"assistant","content":"chicken cross the road?"},"done":false}
{"model":"google/gemini-2.5-pro","created_at":"2026-06-11T07:44:00Z","done":true,"total_duration":450000000,"load_duration":1000000,"prompt_eval_count":10,"eval_count":12}
```

---

### 2. OpenAI Chat Completion (`POST /v1/chat/completions`)

Standard Server-Sent Events (SSE) stream format or synchronous JSON response.

#### Request Example (Streaming)
```bash
curl -X POST -H "Content-Type: application/json" -H "Authorization: Bearer test-token" \
  http://localhost:11434/v1/chat/completions -d '{
  "model": "google/gemini-2.5-pro",
  "messages": [
    {"role": "system", "content": "You are a helpful coding assistant."},
    {"role": "user", "content": "Write a quick hello world in Go."}
  ],
  "stream": true
}'
```

#### SSE Chunks
```text
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1781163840,"model":"google/gemini-2.5-pro","choices":[{"index":0,"delta":{"content":"package "},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1781163840,"model":"google/gemini-2.5-pro","choices":[{"index":0,"delta":{"content":"main"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1781163840,"model":"google/gemini-2.5-pro","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
```

---

## ⚠️ Error Handling Envelope

When an error occurs upstream or internally (e.g. project credentials failure, rate limits, or context budget overflows), the gateway translates the error to fit the specific client dialect's expectations.

### Ollama Error Envelope
Ollama clients expect simple error bodies:
```json
{
  "error": "unsupported publisher \"invalid\": no Vertex AI adapter implemented in this gateway"
}
```

### OpenAI Error Envelope
OpenAI-compatible clients expect a nested, structured error block:
```json
{
  "error": {
    "message": "Vertex AI capacity overloaded. Retries exhausted.",
    "type": "server_error",
    "param": null,
    "code": "upstream_overloaded"
  }
}
```
The gateway automatically maps standard HTTP response codes according to the error context:
*   `401 Unauthorized` — Invalid bearer tokens or missing auth headers.
*   `413 Request Entity Too Large` — Body limits overruns.
*   `429 Too Many Requests` — Over quota limits.
*   `502 Bad Gateway` — Upstream errors (or retries exhausted on overloaded servers).
*   `504 Gateway Timeout` — Network connection timing out.
