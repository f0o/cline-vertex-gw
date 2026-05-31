# API Reference

`cline-vertex-gw` exposes two distinct API client interfaces: the **Ollama Dialect** (`/api/*`) and the **OpenAI Dialect** (`/v1/*`).

---

## 1. Ollama Dialect (`/api/*`)

This interface is fully compatible with standard Ollama API clients. It is specifically designed to support Ollama auto-discovery, allowing tools like VS Code Cline to automatically populate their model pickers.

### `GET /api/tags`

Lists all available and enabled models hosted on Vertex AI, formatted as local Ollama model tags.

*   **Endpoint:** `/api/tags`
*   **Method:** `GET`
*   **Response Format:** `JSON`
*   **Example Curl:**
    ```bash
    curl -s http://127.0.0.1:11434/api/tags | json_pp
    ```

### `POST /api/chat`

Executes a chat completion request using the Ollama-compatible structure.

*   **Endpoint:** `/api/chat`
*   **Method:** `POST`
*   **Response Format:** `NDJSON` (Newline Delimited JSON) — streams JSON chunks line-by-line, ending with a final metrics object.
*   **Payload Example:**
    ```json
    {
      "model": "claude-3-5-sonnet",
      "messages": [
        {"role": "system", "content": "be terse"},
        {"role": "user", "content": "hi"}
      ],
      "stream": true,
      "options": {
        "temperature": 0.3,
        "top_p": 0.9,
        "top_k": 40,
        "num_predict": 1024,
        "stop": ["</done>"]
      }
    }
    ```

### `POST /api/generate`

Executes a single-turn completion request without conversation history wrapping.

*   **Endpoint:** `/api/generate`
*   **Method:** `POST`
*   **Response Format:** `NDJSON`
*   **Note:** Does not support multimodal attachments. Use `/api/chat` instead for vision/media.

---

## 2. OpenAI Dialect (`/v1/*`)

This interface is compatible with any standard OpenAI Chat Completions client, including LiteLLM, langchain, python/node official `openai` SDKs, and Continue.

### `GET /v1/models`

Lists available models formatted in standard OpenAI schema.

*   **Endpoint:** `/v1/models`
*   **Method:** `GET`
*   **Response Format:** `JSON`
*   **Example Response:**
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

Executes a standard OpenAI-compatible chat completion.

*   **Endpoint:** `/v1/chat/completions`
*   **Method:** `POST`
*   **Response Format:** `text/event-stream` if streaming; `JSON` if non-streaming.
*   **Payload Example:**
    ```json
    {
      "model": "gemini-2.0-flash",
      "messages": [
        {"role": "system", "content": "be terse"},
        {"role": "user", "content": "hi"}
      ],
      "stream": true,
      "temperature": 0.3,
      "top_p": 0.9,
      "max_tokens": 1024,
      "stop": ["</done>"]
    }
    ```
*   **Content Parameter Flexibility:** `content` is accepted in either standard form: a plain string (`"content": "hi"`) or a parts array (`"content": [{"type": "text", "text": "hi"}]`).

---

## 3. Supported Model Namespaces

The gateway dynamically routes requests to the correct Vertex AI endpoint based on the model ID. The model ID namespace (prefix) determines which publisher adapter processes the request:

| Model ID Prefix | Upstream Publisher | Routing Strategy | Example Model IDs |
|---|---|---|---|
| `gemini-` / `gemma-` | Google | Google GenAI SDK | `gemini-2.0-flash`, `gemini-2.5-pro`, `gemma-2b-it` |
| `claude-` | Anthropic | Anthropic Messages API over Raw Predict | `claude-3-5-sonnet`, `claude-3-5-haiku`, `claude-3-opus` |
| `command-` | Cohere | Cohere `/chat` over Raw Predict | `command-r`, `command-r-plus` |
| (Any MaaS suffix: `-maas`) | Meta, Mistral, Jamba, DeepSeek, Qwen | OpenAI-compatible over Raw Predict | `llama-3.3-70b-instruct-maas`, `mistral-large-instruct-maas`, `deepseek-r1-maas`, `qwen-2.5-coder-72b-instruct-maas` |

---

## 4. Feature Support Matrix

While both client surfaces share the same upstream code path, certain metadata and capabilities vary slightly due to dialect specifications:

| Client Surface Feature | Ollama `/api/*` | OpenAI `/v1/*` |
|---|---|---|
| Auto-discovery / Tag listing | ✅ (`/api/tags`) | ✅ (`/v1/models`) |
| Interactive System prompts | ✅ | ✅ |
| Custom parameters (`temperature`, `top_p`, `stop`, etc.) | ✅ (`options` block) | ✅ (root fields) |
| Streaming tool-call deltas | ⚠️ (Emitted on terminal `Done` frame) | ✅ (Delta-by-delta SSE) |
| Multimodal input media | ✅ (`images` string array) | ✅ (`image_url` parts list) |
| Polymorphic `input_audio` | ❌ | ✅ |
| Live usage block on final chunk | ❌ | ✅ |
| Custom Bearer Token Authentication | ⚠️ (Requires manual client config) | ✅ (Standard headers) |
