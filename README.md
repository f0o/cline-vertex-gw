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

## 📖 Enhanced Documentation

Our comprehensive documentation is hosted on GitHub Pages:
👉 **[https://f0o.github.io/cline-vertex-gw](https://f0o.github.io/cline-vertex-gw)**

If you are browsing on GitHub, you can navigate straight to the Markdown files in the [`docs/`](./docs) directory:

- 🚀 **[Quick Start Guide](./docs/quickstart.md)** — Credentials setup, Docker run, local build, and client configuration.
- ⚙️ **[Configuration Reference](./docs/configuration.md)** — All `GW_*` environment variables, Application Default Credentials, and administrative/operator endpoints.
- 🛠️ **Features & Deep Dives:**
  - **[Multimodal Input](./docs/features/multimodal.md)** — Images, Audio, Video, and PDF Document translation, magic-bytes sniffing, and upstream publisher gating.
  - **[Tool & Function Calling](./docs/features/tool_calling.md)** — Translating inbound tools, processing streaming/non-streaming responses, and handling tool outputs.
  - **[Metrics & Cost Estimation](./docs/features/cost_metrics.md)** — Real-time USD cost estimation using GCP Billing API, dynamic routing tiers (`flex`/`priority`), and Prometheus metrics.
  - **[Token-Cost Optimization Pipeline](./docs/features/optimization.md)** — Slide-trimming, whitespace normalizers, interactive tool-result truncations, and image/env-block deduplications.
- 🔌 **[API Reference](./docs/api_reference.md)** — Detailed specification of all Ollama (`/api/*`) and OpenAI (`/v1/*`) endpoints, alongside a comparative feature matrix.
- 💻 **[Development & Contribution](./docs/development.md)** — Local development makefiles, test configurations, and CI/CD pipelines.

---

## ⚡ Quick Showcase

Get the gateway up and running locally in three simple steps:

### 1. Configure Credentials
The gateway authenticates with Google Cloud using Application Default Credentials (ADC). Set your GCP Project ID and region:
```bash
export GOOGLE_CLOUD_PROJECT="your-gcp-project-id"
export GOOGLE_CLOUD_LOCATION="us-central1"
```

### 2. Run the Gateway (via Docker)
```bash
docker run -d \
  -p 11434:11434 \
  -v ~/.config/gcloud:/root/.config/gcloud:ro \
  -e GOOGLE_CLOUD_PROJECT="$GOOGLE_CLOUD_PROJECT" \
  -e GOOGLE_CLOUD_LOCATION="$GOOGLE_CLOUD_LOCATION" \
  ghcr.io/f0o/cline-vertex-gw:latest
```

### 3. Verify in Your Client (e.g., VS Code Cline)
1. Open Cline's settings.
2. Select **Ollama** as the API Provider.
3. Set **Ollama Base URL** to `http://localhost:11434`.
4. The **Model** picker will automatically populate with all available models from Vertex AI (e.g. `claude-3-5-sonnet`, `gemini-2.0-flash`)!

---

## ⚖️ License

MIT License — see [`LICENSE`](./LICENSE) for full details.
