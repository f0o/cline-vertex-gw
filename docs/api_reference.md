# API Reference Manual

`cline-vertex-gw` exposes two distinct API dialect surfaces: the **Ollama Dialect** (`/api/*`) and the **OpenAI Dialect** (`/v1/*`).

Both client surfaces share the same backend routing and optimization logic, allowing standard clients to communicate transparently with Google Cloud Vertex AI.

---

## 1. Ollama Dialect (`/api/*`)

Exposes paths compatible with standard local Ollama instances, allowing tools like VS Code Cline to perform automated model discovery and completions natively.

### `GET /api/tags`
Lists all available models hosted on Vertex AI, formatted as local Ollama tags.

- **Endpoint:** `/api/tags`
- **Method:** `GET`
- **Response Format:** `application/json`
- **Example Command:**
  ```bash
  curl -s http://127.0.0.1:11434/api/tags | json_pp
  ```
- **Example Response:**
  ```json
  {
    "models": [
      {
        "name": "gemini-2.0-flash",
        "model": "gemini-2.0-flash",
        "details": {
          "parent_model": "",
          "format": "gguf",
          "family": "gemini",
          "families": ["gemini"],
          "parameter_size": "unknown",
          "quantization_level": "unknown"
        }
      },
      {
        "name": "claude-3-5-sonnet",
        "model": "claude-3-5-sonnet",
        "details": {
          "parent_model": "",
          "format": "gguf",
          "family": "claude",
          "families": ["claude"],
          "parameter_size": "unknown",
          "quantization_level": "unknown"
        }
      }
    ]
  }
  ```

### `POST /api/chat`
Executes an interactive, multi-turn streaming chat completion.

- **Endpoint:** `/api/chat`
- **Method:** `POST`
- **Payload Format:** `application/json`
- **Response Format:** `NDJSON` (Newline Delimited JSON) — streams individual JSON tokens, ending with a final metrics frame.
- **Example Payload:**
  ```json
  {
    "model": "gemini-2.0-flash",
    "messages": [
      {"role": "system", "content": "respond in 3 words"},
      {"role": "user", "content": "hello"}
    ],
    "stream": true,
    "options": {
      "temperature": 0.3,
      "top_p": 0.9,
      "num_predict": 1024,
      "stop": ["\n"]
    }
  }
  ```

### `POST /api/generate`
Executes a single-turn raw text generation request.

- **Endpoint:** `/api/generate`
- **Method:** `POST`
- **Payload Format:** `application/json`
- **Response Format:** `NDJSON`
- **Example Payload:**
  ```json
  {
    "model": "claude-3-haiku",
    "prompt": "write a haiku about compiling Go code",
    "stream": false
  }
  ```

---

## 2. OpenAI Dialect (`/v1/*` - Recommended & Superior)

Exposes standard OpenAI-compatible paths. This interface is **architecturally superior** to the Ollama compatibility fallback:
- **Superior Streaming Tool Calling:** Streams function call arguments token-by-token in real-time, allowing clients to show tool calls dynamically.
- **Real-Time Stream Usage:** Emits standard usage metric blocks on the final stream chunk for precise billing tracking.
- **Ecosystem Compatibility:** Plugs natively and robustly into LiteLLM, langchain, standard OpenAI SDKs, Continue, and Cline's OpenAI Compatible provider.

### `GET /v1/models`
Lists available models formatted in OpenAI-compatible lists.

- **Endpoint:** `/v1/models`
- **Method:** `GET`
- **Response Format:** `application/json`
- **Example Command:**
  ```bash
  curl -s http://127.0.0.1:11434/v1/models | json_pp
  ```
- **Example Response:**
  ```json
  {
    "object": "list",
    "data": [
      {
        "id": "gemini-2.0-flash",
        "object": "model",
        "created": 1747737600,
        "owned_by": "google"
      },
      {
        "id": "claude-3-5-sonnet",
        "object": "model",
        "created": 1747737600,
        "owned_by": "anthropic"
      }
    ]
  }
  ```

### `POST /v1/chat/completions`
Executes an OpenAI-compatible chat completion.

- **Endpoint:** `/v1/chat/completions`
- **Method:** `POST`
- **Payload Format:** `application/json`
- **Response Format:** `text/event-stream` if streaming; standard `application/json` if non-streaming.
- **Example Payload:**
  ```json
  {
    "model": "claude-3-5-sonnet",
    "messages": [
      {"role": "user", "content": "hi"}
    ],
    "stream": true,
    "temperature": 0.3,
    "max_tokens": 1024
  }
  ```

---

## 3. Supported Model Namespaces

The gateway splits your model input identifier into a publisher prefix and a bare identifier. It routes requests dynamically to the appropriate publisher adapter on Vertex AI:

| Model ID Namespace Prefix | Upstream Publisher | Routing Strategy | Example Model IDs |
|---|---|---|---|
| `gemini-` / `gemma-` | Google | Google GenAI SDK | `gemini-2.0-flash`, `gemini-2.5-pro`, `gemma-2-9b-it` |
| `claude-` | Anthropic | Anthropic Messages API (`:streamRawPredict`) | `claude-3-5-sonnet`, `claude-3-5-haiku`, `claude-3-opus` |
| `command-` | Cohere | Cohere `/chat` API (`:streamRawPredict`) | `command-r`, `command-r-plus` |
| (Any `-maas` model suffix) | Meta, Mistral, Jamba, DeepSeek, Qwen | OpenAI-compatible endpoint (`:streamRawPredict`) | `llama-3.3-70b-instruct-maas`, `mistral-large-instruct-maas`, `deepseek-r1-maas` |

---

## 4. Dialect Feature Matrix

While both surface handlers share the same upstream code paths, the specific capabilities of each dialect vary due to upstream specification differences:

| Capability / Feature | Ollama Dialect (`/api/*`) | OpenAI Dialect (`/v1/*`) |
|---|:---:|:---:|
| **Dynamic Discovery** | ✅ (`/api/tags`) | ✅ (`/v1/models`) |
| **Streaming Completions** | ✅ (NDJSON) | ✅ (Server-Sent Events) |
| **Interactive System Prompts** | ✅ | ✅ |
| **Bearer Token Security** | ⚠️ (Requires manual client config) | ✅ (Standard Authorization headers) |
| **Multimodal Base64 Inputs** | ✅ (`images` string array) | ✅ (`image_url` parts array) |
| **Polymorphic Audio Inputs** | ❌ | ✅ (`input_audio` schema) |
| **Streaming Tool-Call Deltas** | ⚠️ (Emitted on terminal Done frame) | ✅ (Real-time delta-by-delta SSE) |
| **Live Usage Blocks** | ❌ | ✅ (On final stream chunk) |
