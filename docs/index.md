# cline-vertex-gw

An Ollama-compatible **and** OpenAI-compatible HTTP gateway in front of Google Cloud Vertex AI.

`cline-vertex-gw` lets any client that speaks either dialect — Ollama (e.g. [Cline](https://cline.bot), `ollama` CLI, `llm`) or OpenAI Chat Completions (LiteLLM, LangChain, official `openai` SDKs, Continue, Cline's "OpenAI Compatible" provider, etc.) — transparently call Vertex-hosted models: Gemini, Gemma, Claude, Llama, Mistral / Mixtral / Codestral, Jamba, Cohere Command R/R+, DeepSeek, Qwen, and Nvidia-hosted MaaS models — without changing the client.

```
                       /api/chat        ┌──────────────────┐   Vertex REST    ┌────────────┐
┌──────────┐  Ollama   /api/tags        │                  │   rawPredict     │            │
│  Cline   │ ─────────►/api/generate ──►│                  │ ───────────────► │            │
└──────────┘                            │ cline-vertex-gw  │   streamGenerate │ Vertex AI  │
┌──────────┐  OpenAI   /v1/models       │                  │   streamRaw      │            │
│ LiteLLM  │ ─────────►/v1/chat/        │                  │ ───────────────► │            │
└──────────┘           completions      └──────────────────┘                  └────────────┘
```

---

## Features

- **Two client dialects, one upstream.** All endpoints share the same Vertex translation pipeline, retry logic, metrics, and auth — pick whichever shape your client prefers.
- **Ollama surface** (auto-discovery; great for Cline's model picker):
  - `GET /api/tags` - returns enabled Vertex models formatted as local Ollama tags.
  - `POST /api/chat` - translates Ollama chat format to Vertex AI, supporting full streaming (NDJSON format).
  - `POST /api/generate` - single-turn generation support.
- **OpenAI-compatible surface** (bearer token support; great for LiteLLM, langchain, standard SDKs):
  - `GET /v1/models` - lists enabled models in standard OpenAI schema.
  - `POST /v1/chat/completions` - supports system prompts, custom parameters, inline `image_url` parts (vision), and streaming (`text/event-stream`).
- **Dynamic publisher dispatch.** Calls the correct Vertex REST endpoint for each model family:
  - Gemini & Gemma: uses Google's standard Generative AI protocol.
  - Anthropic Claude 3 / 3.5: maps to Vertex's native Anthropic Messages endpoint (`/publishers/anthropic/models/...:streamRawPredict`).
  - Cohere Command R / R+: maps to Vertex's native Cohere `/chat` endpoint (`/publishers/cohere/models/...:streamRawPredict`).
  - Llama 3 / 3.1 / 3.2 / 3.3, Mistral / Codestral, Jamba, DeepSeek-R1 / Qwen, and Nvidia MaaS models: routes via Vertex Llama/Mistral/MaaS raw predicts.
- **Robust Tool Calling / Function Calling.** Integrates complex tool calls on both Ollama and OpenAI surfaces. Translates inbound tool definitions, processes assistant tool calls, and returns formatted tool results seamlessly.
- **Unified token-cost optimization pipeline.** Employs 5 progressive profiles (from raw Pass-Through to Extreme Squeeze) utilizing:
  - White-space / comment normalizers.
  - Redundant environment block collapsing (exempting the latest turn to preserve IDE context).
  - Multimodal part-level caching and deduplication.
  - Interactive tool call pruning & tool-result truncation (head/tail preservation).
  - Sliding context trimming.
- **Prometheus Metrics & Cost Tracking.** Automatically tracks estimated USD cost, prompt/cache/completion token counts, latencies, compression bytes saved, and cache hit ratios.
- **Production Hardened.** Full panic recovery, body size limits, configurable connection pools, automatic retries with exponential backoff, and bearer-token authentication.

---

## Why Two Client Surfaces?

Clients in the LLM agent ecosystem usually default to either the Ollama interface or the OpenAI-compatible interface. Having both surfaces in `cline-vertex-gw` provides ultimate flexibility:

| Capability | Ollama `/api/*` | OpenAI `/v1/*` |
|---|---|---|
| Auto-discovery / Tag listing | ✅ (`/api/tags`) | ✅ (`/v1/models`) |
| Native Cline model picker support | ✅ | ⚠️ (Requires manually adding custom profiles) |
| Per-request Bearer token authentication | ❌ (Client-side limitations) | ✅ (Using `Authorization` header) |
| Multimodal input (Images, Audio, Video, PDF) | ✅ (`images` / magic sniffing) | ✅ (`image_url` / polymorphic `input_audio`) |
| Streaming `done_reason` / Ollama token counts | ✅ | n/a |
| Streaming `usage` block on final chunk | n/a | ✅ |

Both share the same upstream code path: routing across publishers, retries, metrics, auth, panic recovery, and body-size limits. Switching dialects is just a client-side config choice.

*Note:* If you only ever call a fixed set of MaaS models (Llama, Mistral, Jamba, DeepSeek, Qwen) you can also point your OpenAI client straight at Vertex's [native OpenAI-compatible endpoint](https://cloud.google.com/vertex-ai/generative-ai/docs/multimodal/call-vertex-using-openai-library) and skip this gateway entirely. Use this gateway when you want a single endpoint that routes across Gemini + Anthropic + Cohere + MaaS by model id, or when you want Cline's Ollama-style auto-discovery.
