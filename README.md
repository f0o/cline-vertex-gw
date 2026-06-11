# Cline Vertex AI Gateway

[![Go Report Card](https://goreportcard.com/badge/github.com/f0o/cline-vertex-gw)](https://goreportcard.com/report/github.com/f0o/cline-vertex-gw)
[![Docker Image Version](https://img.shields.io/github/v/release/f0o/cline-vertex-gw?label=Docker&logo=docker)](https://github.com/f0o/cline-vertex-gw/pkgs/container/cline-vertex-gw)
[![MIT License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

An **Ollama and OpenAI-compatible API translation gateway and context-optimization proxy** designed specifically for agentic developer workflows (such as Cline, Roo Code, and SWE-agent) running against **Google Cloud Vertex AI** models.

The gateway bridges client-side expectations of local Ollama or cloud OpenAI specifications with the production-grade security, enterprise compliance, and low cost of Google's Vertex AI model garden.

---

## 🚀 Key Advantages

*   **100% Drop-In Client Compatibility**: Completely compatible with Ollama `/api/*` endpoints and standard OpenAI `/v1/*` specifications. Auto-discovers and exposes Vertex AI and model garden endpoints seamlessly.
*   **Dual-Client Surface**: Exposes Ollama's auto-discovery (`/api/tags`) for client model selection dropdowns while providing OpenAI-style `/v1/chat/completions` with bearer token auth.
*   **11 Upstream Publisher Families**: Houses unified translation adapters for Google (Gemini/Gemma), Anthropic (Claude 3/3.5/Opus/Sonnet/Haiku), Cohere (Command R/R+), Meta (Llama), Mistral AI, xAI, DeepSeek, Qwen, MiniMax, Moonshot, and GLM (Zhipu AI).
*   **Production-Grade Prompt Optimization**: Executes a 14-stage in-flight context compression and optimization pipeline, providing **up to 70-90% token savings** and near-flat historical costs on warm sessions.
*   **Real-Time Price and Billing Telemetry**: Integrates a dynamic Hybrid Cost Engine that scrapes GCP's live Cloud Billing Catalog API and static HTML product pages to estimate session USD consumption with 100% precision.
*   **Google-Native Routing Tiers**: Exposes full header-level and context-propagated support for Google Cloud's discounted **Flex/Batch** capacity pools alongside **Standard** and **Priority** service tiers.

---

## 🗺️ System Architecture

```
                                 ┌─────────────────────────────────┐
                                 │       Ollama/OpenAI Client      │
                                 │        (e.g., Cline IDE)        │
                                 └────────────────┬────────────────┘
                                                  │
                                                  ▼ HTTP
                                 ┌─────────────────────────────────┐
                                 │      cline-vertex-gw Proxy      │
                                 └────────────────┬────────────────┘
                                                  │
                                                  ▼ [14-Stage Optimization Pipeline]
         ┌───────────────────┬────────────────────┼───────────────────┬───────────────────┐
         │ (Normalize)       │ (Env Blocks)       │ (Middle Elide)    │ (Deep Compact)    │ (Budget Trim)
         ▼                   ▼                    ▼                   ▼                   ▼
┌─────────────────┐ ┌─────────────────┐  ┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
│Strip BOMs/CRLF  │ │Collapse stale   │  │Truncate tool    │ │Compress older   │ │Slide history to │
│Normalize spaces │ │Cline env detail │  │outputs to head  │ │turns to 100-byte│ │match character  │
│and blank lines  │ │to placeholder   │  │and tail windows │ │high-density ptr │ │soft-limits      │
└─────────────────┘ └─────────────────┘  └─────────────────┘ └─────────────────┘ └─────────────────┘
                                                  │
                                                  ▼ [Unified Publisher Dispatch]
         ┌───────────────────┬────────────────────┼───────────────────┐
         │                   │                    │                   │
         ▼                   ▼                    ▼                   ▼
┌─────────────────┐ ┌─────────────────┐  ┌─────────────────┐ ┌─────────────────┐
│     Google      │ │    Anthropic    │  │     Cohere      │ │  OpenAI-Compat  │
│  (Gemini/Gemma) │ │ (Claude 3/3.5)  │  │ (Command R/R+)  │ │ (DeepSeek/Meta) │
└────────┬────────┘ └────────┬────────┘  └────────┬────────┘ └────────┬────────┘
         │                   │                    │                   │
         └───────────────────┴─────────┬──────────┴───────────────────┘
                                       │
                                       ▼ REST / gRPC
                        ┌───────────────────────────────┐
                        │      Google Cloud Vertex      │
                        │           AI API              │
                        └───────────────────────────────┘
```

---

## ⚡ Quickstart

### 1. Authenticate with Google Cloud
The gateway leverages standard Google **Application Default Credentials (ADC)**. Authenticate your local machine using the Google Cloud CLI:

```bash
gcloud auth application-default login
```

### 2. Run the Gateway
Run the pre-built Docker container locally, mapping it to Ollama's standard port (`11434`):

```bash
docker run -d \
  -p 11434:11434 \
  -e GOOGLE_CLOUD_PROJECT="your-gcp-project-id" \
  -e GOOGLE_CLOUD_LOCATION="us-central1" \
  -e GW_PROFILE="balanced" \
  -v ~/.config/gcloud:/home/nonroot/.config/gcloud:ro \
  ghcr.io/f0o/cline-vertex-gw:latest
```

### 3. Connect Cline
1. In Cline, open **Settings** (Gear Icon) -> **Provider**.
2. Select **Ollama** as your provider.
3. Keep the base URL as default: `http://localhost:11434`.
4. The model dropdown will auto-populate with all discovered Vertex AI models (e.g. `google/gemini-2.5-pro`, `anthropic/claude-3-5-sonnet-v2`, `deepseek-ai/deepseek-r1`). Select your preferred model and start coding!

---

## 📘 Comprehensive Documentation

To explore our extensive configuration tables, deployment options, features guides, and deep dives into the prompt optimization pipeline, refer to the full documentation suite:

*   **[Introduction & Core Concepts](docs/index.md)**: Gateway motivations, system-level design patterns, and comparative capabilities.
*   **[Deployment & Quickstart](docs/quickstart.md)**: Local binary compilation, advanced Docker compose setups, and detailed client configurations.
*   **[Configuration Reference](docs/configuration.md)**: Descriptions, limits, and profiles of all environment parameters (`GW_*` and global binds).
*   **[API & Endpoint References](docs/api_reference.md)**: Comparative Ollama/OpenAI wire specs, discovery schemas, and testing `curl` requests.
*   **[Features Guide](docs/features/index.md)**:
    *   **[Multimodal Processing](docs/features/multimodal.md)**: Dynamic capability gating and mime-type sniffing for video, audio, image, and PDF inputs.
    *   **[Native Tool Calling](docs/features/tool_calling.md)**: Schema translations, parallel tool execution support, and ID-based alignment.
    *   **[Billing & Metrics](docs/features/cost_metrics.md)**: Dynamics scraper details, routing tier pricing overrides, and Prometheus metrics exporting.
*   **[Prompt Optimization Pipeline](docs/optimization/overview.md)**:
    *   **[0. Loop Break Trap](docs/optimization/loopbreak.md)**: Mitigating LLM scolding loops.
    *   **[0a. Active Tool Pruning](docs/optimization/active_tool_pruning.md)**: Shedding unused auxiliary tools.
    *   **[0b. Stale Tool Pruning](docs/optimization/prune_tools.md)**: Replacement of superseded inspects.
    *   **[1a. Normalize Whitespace](docs/optimization/normalize.md)**: Code-safe format standardization.
    *   **[1b. Cache Aligner](docs/optimization/cache_aligner.md)**: Stabilizing prefix hits.
    *   **[2. Collapse Env Blocks](docs/optimization/envblocks.md)**: Shunting stale Cline IDE state snapshots.
    *   **[2b. Tool Result Truncation](docs/optimization/toolresult.md)**: Dual-window progressive observation masking.
    *   **[2b1. Write Action Elision](docs/optimization/write_elision.md)**: Compressing historical file modifications.
    *   **[2c. Deep Compaction](docs/optimization/deepcompact.md)**: Semantic compaction of cold history.
    *   **[3. Context Budget Trim](docs/optimization/budget.md)**: Soft budget-sliding char trimming.
    *   **[4. Duplicate Block Dedup](docs/optimization/dedup.md)**: Multi-turn whole-block back-pointing deduplication.
    *   **[4b. Substring Block Dedup](docs/optimization/dedup_substring.md)**: Multi-turn substring deduplication.
    *   **[5. Function Call Alignment](docs/optimization/align.md)**: Ensuring strict role-alternation compliance.
    *   **[Lossless CCR Loop](docs/optimization/ccr.md)**: FSCache structures, retrieve tools, and loop-bypass invariants.

---

## ⚖️ License

Distributed under the MIT License. See `LICENSE` for more information.
