# Introduction & Core Concepts

## What is Cline Vertex AI Gateway?

**Cline Vertex AI Gateway** is a high-performance proxy gateway that translates Ollama and OpenAI-compatible API requests into native Google Cloud Vertex AI API requests.

It acts as a local drop-in wrapper, enabling developer agents and standard LLM clients (such as **Cline**, **Roo Code**, **SWE-agent**, and other development tools native to the Ollama and OpenAI ecosystems) to transparently connect to Google's robust, enterprise-grade model garden on Vertex AI.

---

## Why This Project Exists

Developer agents like Cline expect to interact with either a local Ollama instance (to benefit from auto-discovery `/api/tags` and zero-setup deployment) or standard OpenAI/Anthropic cloud endpoints. 

However, enterprise teams and developers frequently want to use **Google Cloud Vertex AI** because of:
1.  **Production Stability & SLAs**: Enterprise uptime, regional compliance, and data sovereignty guarantees.
2.  **No Data Training Policy**: Google Cloud guarantees that your prompt data and code outputs are never used to train future public foundation models.
3.  **Extremely Aggressive Pricing**: Vertex AI's PayGo and discounted Batch/Flex routing models offer some of the most competitive pricing in the industry.
4.  **Google-Managed Publisher Models**: Vertex AI manages not only Google Gemini, but also third-party publisher models like Anthropic Claude, Meta Llama, Cohere Command, and more in a single unified API surface.

The **Cline Vertex AI Gateway** bridges this gap. It acts as an adapter layer so you can point any Ollama-native or OpenAI-compatible client directly to your local gateway, which then authenticates with GCP, translates the request shapes dynamically, runs prompt optimization algorithms to shave off 80% of your costs, and proxies the stream back in real-time.

---

## System Architecture

The gateway is built in **Go** as a dual-surface, highly concurrent translation proxy. It receives standard client requests, applies a 14-stage prompt optimization pipeline, dispatches to the correct publisher-specific endpoint on Google Cloud, and pipes SSE/NDJSON events back to the client.

```
       ┌────────────────────────┐         ┌────────────────────────┐
       │     Ollama Client      │         │     OpenAI Client      │
       │     (e.g. Cline)       │         │    (e.g. LiteLLM)      │
       └───────────┬────────────┘         └───────────┬────────────┘
         NDJSON    │                                  │ SSE
                   ├──────────────────────────────────┘
                   ▼
┌──────────────────────────────────────────────────────────────────┐
│                      cline-vertex-gw Proxy                       │
├──────────────────────────────────────────────────────────────────┤
│                                                                  │
│  1. HTTP Server & Rate-Limit Middleware                          │
│     - Graceful Shutdown, Header/Body Timeout Caps                │
│     - Bearer Token Auth via GATEWAY_AUTH_TOKEN                   │
│                                                                  │
│  2. 14-Stage Prompt Optimization Pipeline                        │
│     - Loopbreak, Whitespace & System Prompt Alignment            │
│     - Env Block Collapsing, Progressive Masking, Write Elision   │
│     - Slid-Window Trimming, Whole-Block & Substring Dedup       │
│                                                                  │
│  3. Publisher Dispatcher & Translation Adapters                 │
│     - Google (Gemini/Gemma via genai SDK)                        │
│     - Anthropic Messages API (:streamRawPredict)                 │
│     - Cohere /chat API                                           │
│     - OpenAI-Compatible MaaS (DeepSeek, Llama, Qwen, GLM, etc.)  │
│                                                                  │
│  4. Dynamic Billing Engine & Metrics                             │
│     - Dynamic SKU scraper + static public HTML table parser      │
│     - Prometheus cost/metric exporter                            │
│                                                                  │
└──────────────────────────────────┬───────────────────────────────┘
                                   │ HTTPS REST / gRPC
                                   ▼
┌──────────────────────────────────────────────────────────────────┐
│                      Google Cloud Platform                       │
├──────────────────────────────────────────────────────────────────┤
│                                                                  │
│                      Vertex AI LLM Gateway                       │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

---

## High-Level Capabilities

*   **Dual API Surfaces**: One backend engine, two front-facing client interfaces. The gateway exposes Ollama's model discovery (`GET /api/tags`), streaming chat completions (`POST /api/chat`), and single-turn text completions (`POST /api/generate`), as well as OpenAI's specifications (`GET /v1/models` and `POST /v1/chat/completions`).
*   **11-Vendor Model Discovery**: Leverages standard Google credentials to automatically query Vertex's endpoints. Supported publishers include `google`, `anthropic`, `cohere`, `meta`, `mistralai`, `ai21`, `deepseek-ai`, `qwen`, `nvidia`, `xai` (Grok), and `zhipuai` (GLM).
*   **In-Flight Prompt Compression**: Developer agents are notoriously chatty, repeating massive system prompts, file dumps, and environment snapshots across every single message turn. The gateway runs a pipeline to shrink, dedup, trim, and collapse history, saving thousands of dollars per developer.
*   **Dynamic Cost Scraping & Attribution**: Operators do not need to manually configure rate cards. The gateway scrapes GCP's live billing API alongside static public pricing pages on startup, attribute costs per token-type (standard input, cached input, output) and routing tier, and exposes them to Prometheus.
