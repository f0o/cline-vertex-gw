# cline-vertex-gw

An Ollama-compatible **and** OpenAI-compatible HTTP gateway in front of
Google Cloud Vertex AI.

Lets any client that speaks either dialect — Ollama (e.g. [Cline](https://cline.bot),
`ollama` CLI, `llm`) or OpenAI Chat Completions (LiteLLM, LangChain,
official `openai` SDKs, Continue, Cline's "OpenAI Compatible" provider,
etc.) — transparently call Vertex-hosted models: Gemini, Gemma, Claude,
Llama, Mistral / Mixtral / Codestral, Jamba, Cohere Command R/R+,
DeepSeek, Qwen, and Nvidia-hosted MaaS models — without changing the client.

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

- **Two client dialects, one upstream.** All endpoints share the same
  Vertex translation pipeline, retry logic, metrics, and auth — pick whichever
  shape your client prefers.
- **Ollama surface** (auto-discovery; great for Cline's model picker):
  - **`GET /api/tags`** — discovers every chat-capable model your project can
    access across all supported publishers (concurrent fan-out, per-publisher
    fallback through project-regional → us-central1 → global catalog).
  - **`POST /api/chat`** — Ollama chat completions with streaming NDJSON.
  - **`POST /api/generate`** — Ollama single-turn generate, also streaming.
- **OpenAI surface** (drop-in for OpenAI-shaped clients, supports per-request
  `Authorization: Bearer …` headers):
  - **`GET /v1/models`** — same discovery, OpenAI envelope.
  - **`POST /v1/chat/completions`** — OpenAI Chat Completions; SSE streaming
    with the standard `data: {...}\n\ndata: [DONE]` framing or non-streaming
    JSON, gated by the `stream` field.
- **Provider adapters** for: Google (Gemini/Gemma via genai SDK), Anthropic
  (Messages API on `:streamRawPredict`), Cohere (Chat API), and an
  OpenAI-compatible adapter shared by Meta Llama / Mistral / AI21 / DeepSeek /
  Qwen / Nvidia MaaS.
- **Native tool / function calling** end-to-end on both surfaces — `tools`,
  `tool_choice`, `tool_calls`, and `role:"tool"` results are translated into
  each publisher's native shape (Anthropic `tool_use`/`tool_result` blocks,
  OpenAI-compat `tools`/`tool_calls`, Cohere `parameter_definitions`/
  `tool_results`, Gemini `FunctionDeclaration`/`FunctionCall`). See
  [Tool calling](#tool-calling) below.
- **Multimodal (image) inputs** on both surfaces — inline base64 `data:` URLs
  on `/v1/chat/completions`, the Ollama-native `images: [...]` per-message
  field on `/api/chat`, accepting PNG / JPEG / WEBP / GIF. Translated into
  each publisher's native shape (Gemini `InlineData`, Claude `image` block,
  OpenAI-compat `image_url`). Capability-gated per model so a request to a
  text-only model returns a clean 400 instead of failing mid-stream. See
  [Multimodal](#multimodal) below.
- **Per-request retries** with exponential backoff + jitter, only when no
  bytes have been streamed (no duplicate-output hazard).
- **Authenticated mode** via a shared bearer token (protects both `/api/*`
  and `/v1/*`).
- **Hardened HTTP server**: read header timeout, idle timeout, max-header
  bytes, body-size limit, graceful shutdown on SIGTERM, panic recovery.
- **Per-request structured-ish logging** with stable request IDs and a
  done-line carrying tokens/duration/tps/finish reason.

---

## Quick start

### 1. Prerequisites

- A Google Cloud project with the Vertex AI API enabled.
- Application Default Credentials available to the process — e.g.
  `gcloud auth application-default login` for local use, or a service account
  attached to the runtime (GCE, Cloud Run, GKE Workload Identity).
- Go 1.26+ if building from source.

### 2. Build

```bash
go build -o cline-vertex-gw .
```

### 3. Run

```bash
export GOOGLE_CLOUD_PROJECT=my-project
export GOOGLE_CLOUD_LOCATION=global         # or us-central1, us-east5, europe-west4, ...
export GATEWAY_AUTH_TOKEN=$(openssl rand -hex 32)
./cline-vertex-gw
```

The gateway listens on `:11434` by default (the Ollama port). It will print:

```
SECURITY WARNING: GATEWAY_AUTH_TOKEN is not set; the gateway is running UNAUTHENTICATED.
```

…if you skip the token — fine for a local-only setup, **not fine** if anyone
else can reach the port.

### 4. Point a client at it

Pick whichever dialect your client prefers — both surfaces hit the same
Vertex backend, with identical model coverage, retries, and metrics.

#### Option A: Ollama-compatible client (Cline's auto-discovery picker, `ollama` CLI)

In Cline's settings, pick **API Provider: Ollama**, and set:

| Field | Value |
|---|---|
| Base URL | `http://127.0.0.1:11434` |
| Model    | one of the names returned by `GET /api/tags` (e.g. `gemini-2.0-flash`, `claude-opus-4-7`, `llama-3.3-70b-instruct-maas`) |

If you set `GATEWAY_AUTH_TOKEN`, Cline's Ollama provider does **not** support
custom headers out of the box — either use the OpenAI-Compatible option below
(it does), run the gateway behind a reverse proxy that injects the
`Authorization: Bearer <token>` header for you, or run it unauthenticated
bound to `127.0.0.1` (see [Security](#security)).

#### Option B: OpenAI-compatible client (Cline's OpenAI-Compatible provider, LiteLLM, openai SDK)

In Cline's settings, pick **API Provider: OpenAI Compatible**, and set:

| Field | Value |
|---|---|
| Base URL | `http://127.0.0.1:11434/v1` |
| API Key  | your `GATEWAY_AUTH_TOKEN` (or any non-empty value if running unauthenticated) |
| Model ID | one of the IDs returned by `GET /v1/models` (e.g. `gemini-2.0-flash`, `claude-opus-4-7`, `llama-3.3-70b-instruct-maas`) |

The OpenAI-Compatible provider sends the API key as `Authorization: Bearer
<key>`, which the gateway's auth middleware verifies. Unlike the Ollama
provider it does **not** auto-discover models — you must enter the model id
manually.

Quick smoke test with `curl`:

```bash
curl -sS http://127.0.0.1:11434/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-2.0-flash",
    "messages": [{"role":"user","content":"say hi in one word"}],
    "stream": false
  }'
```

Or with the official `openai` Python SDK:

```python
from openai import OpenAI
c = OpenAI(base_url="http://127.0.0.1:11434/v1", api_key="…GATEWAY_AUTH_TOKEN…")
r = c.chat.completions.create(
    model="claude-opus-4-7",
    messages=[{"role":"user","content":"hi"}],
)
print(r.choices[0].message.content)
```

> **When to pick which.** Use **Ollama** when you want Cline's model picker
> to auto-populate with every Vertex model your project can see. Use
> **OpenAI Compatible** when you want per-request bearer-token auth, are
> already speaking OpenAI Chat Completions (LiteLLM, LangChain, an existing
> codebase), or are integrating with tooling that doesn't speak Ollama.
> Vertex AI also exposes an
> [OpenAI-compatible Chat Completions endpoint](https://cloud.google.com/vertex-ai/generative-ai/docs/multimodal/call-vertex-using-openai-library)
> natively for several MaaS publishers — if you only call those models and
> already know the model id, you can skip this gateway entirely.

### Tool calling

The gateway forwards native tool / function calling across every publisher
and both client surfaces. There is no configuration to enable — if your
request includes a `tools` array (or the legacy `functions` field on
`/v1/chat/completions`), the gateway translates it into the upstream's
native shape and translates any tool call the model emits back into the
client's wire format.

Supported on both `/v1/chat/completions` and `/api/chat`:

| Inbound (client → gateway) | Translated to (upstream) |
|---|---|
| `tools: [...]` request slot | Anthropic `tools`, OpenAI-compat `tools`, Cohere `tools` (with `parameter_definitions` flattened from JSON Schema), Gemini `FunctionDeclaration` |
| `tool_choice: "auto"\|"none"\|"required"\|{type:"function",...}` | Each publisher's equivalent (Anthropic `tool_choice`, OpenAI `tool_choice`, Cohere `tools` presence/absence, Gemini `FunctionCallingConfig`) |
| Assistant `tool_calls` on a prior turn | Anthropic `tool_use` blocks, OpenAI assistant `tool_calls`, Cohere CHATBOT `tool_calls`, Gemini `FunctionCall` part |
| `role:"tool"` result message | Anthropic `tool_result` block, OpenAI `tool` message, Cohere `tool_results` (lifted to request level on the final turn), Gemini `FunctionResponse` part |

Outbound (gateway → client):

| Outbound | Wire shape |
|---|---|
| Streaming tool call on `/v1/chat/completions` | `delta.tool_calls[]` with `id`, `function.name`, and `function.arguments` (JSON-encoded string per OpenAI spec) — one delta per call, fully assembled. |
| Non-streaming tool call on `/v1/chat/completions` | `choices[0].message.tool_calls[]` with the same shape. |
| Tool call on `/api/chat` (Ollama) | `message.tool_calls[]` on the terminal `Done` frame, with `arguments` as a JSON object (Ollama convention, NOT stringified). |
| `finish_reason` when a tool was called | `"tool_calls"` on `/v1/*`, `done_reason: "tool_use"` on `/api/chat`. Upgraded defensively even when the upstream reports `"stop"` despite emitting a call. |

Quick smoke test with `curl`:

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

The response will include `choices[0].message.tool_calls` and
`finish_reason: "tool_calls"` if the model chose to invoke the tool.

Caveats:

- Cohere does not supply per-call IDs; the gateway synthesizes
  `call_<hex>` ids on outbound. The synthetic id round-trips through
  follow-up `role:"tool"` messages so multi-turn conversations stay
  coherent.
- The `/api/generate` endpoint (legacy single-turn Ollama) has no slot
  for tool calls in its wire shape; if you pass a tool-aware model that
  emits a call against this endpoint, the call is silently dropped.
  Use `/api/chat` or `/v1/chat/completions` instead.
- Model behavior with tools varies — some MaaS models (notably smaller
  Llama variants) will emit prose instead of a tool call even when one
  is clearly indicated. That's a model-quality issue, not a gateway
  bug; pick a model with stronger tool-following (Claude, Gemini,
  Llama-3.3-70B and up).

### Multimodal

The gateway forwards inline image inputs to every publisher that supports
them on Vertex AI. Both client surfaces are supported and decode to the
same internal representation before being re-encoded into the per-publisher
wire shape.

**On `/v1/chat/completions`** — OpenAI parts-array form with inline data
URLs:

```json
{
  "role": "user",
  "content": [
    {"type": "text", "text": "what's in this screenshot?"},
    {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgoAAAANSU…"}}
  ]
}
```

**On `/api/chat`** — Ollama's native per-message `images` field (bare
base64, no `data:` prefix; MIME is sniffed from the magic bytes):

```json
{
  "role": "user",
  "content": "what's in this screenshot?",
  "images": ["iVBORw0KGgoAAAANSU…"]
}
```

Either form decodes to the same `*genai.Part{InlineData: {MIMEType, Data}}`
internal IR and then re-encodes per publisher:

| Publisher | Outbound wire shape |
|---|---|
| **Google** (Gemini 1.5+, Gemma 3) | `inlineData: {mimeType, data}` part — passed through the genai SDK unchanged. |
| **Anthropic** (Claude 3+, 3.5, 4.x, *-thinking) | `{type: "image", source: {type: "base64", media_type, data}}` content block, ordered alongside text blocks. |
| **OpenAI-compat** (Llama 3.2 Vision, Pixtral, Qwen2-VL, llama-3.2-nv-vision-*, Llama 4) | Native `{type:"image_url", image_url:{url:"data:…"}}` parts array on the message. |
| **Cohere** | Not supported on Vertex today (Command-A-Vision is direct-API only). Returns 400 at the gateway boundary. |

**Vision capability gate.** Before any image-bearing request leaves the
gateway, `provider.CheckVisionSupport` checks the resolved publisher/model
combination against an allowlist. Sending an image to a text-only model
(plain `llama-3.1-405b`, `mistral-large`, `command-r-plus`, `jamba`,
`deepseek-v3`, etc.) returns:

```json
{
  "error": {
    "message": "model \"llama-3.3-70b-instruct-maas\" (publisher=\"meta\") does not support image inputs on Vertex AI; use a vision-capable model instead — e.g. gemini-2.0-flash, claude-3-5-sonnet, llama-3.2-90b-vision-instruct-maas, or pixtral-12b",
    "type": "invalid_request_error",
    "code": "model"
  }
}
```

…with HTTP 400. This catches misconfigurations BEFORE we commit response
headers, so the client gets a clean parseable error instead of a
half-formed SSE stream that errors out mid-flight from the upstream.

**Image dedup** (part of the compression pipeline). Cline's
"screenshot-every-turn" workflow ships near-identical PNGs across turns.
The dedup stage hashes image bytes (SHA-256, role+MIME-scoped) and replaces
later duplicates with a text-part placeholder pointing at the earlier
turn. Shipping the same 800 KB screenshot 5 times across a session costs
~800 KB of upload bandwidth instead of 4 MB.

**Limits and safety:**

- Only `image/png`, `image/jpeg`, `image/webp`, `image/gif` are accepted.
  Other MIME types (SVG, TIFF, BMP) return 400.
- Only `data:` URLs are accepted — `https://…` references are rejected as
  an SSRF guard. The gateway never makes outbound HTTP requests to fetch
  user-supplied URLs.
- Per-image and per-request size caps apply (`GW_MAX_IMAGE_BYTES_PER_PART`
  default 10 MiB, `GW_MAX_IMAGE_BYTES_PER_REQUEST` default 32 MiB).
- Images attached to `system`, `developer`, or `tool` messages are
  silently dropped (no upstream supports them as system/tool context).
- The `/api/generate` endpoint doesn't support multimodal at all — use
  `/api/chat` or `/v1/chat/completions`.

Quick smoke test against Gemini Vision:

```bash
IMG_B64=$(base64 -w0 < screenshot.png)
curl -sS http://127.0.0.1:11434/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"gemini-2.0-flash\",
    \"stream\": false,
    \"messages\": [{
      \"role\": \"user\",
      \"content\": [
        {\"type\": \"text\", \"text\": \"describe this image in one sentence\"},
        {\"type\": \"image_url\", \"image_url\": {\"url\": \"data:image/png;base64,${IMG_B64}\"}}
      ]
    }]
  }"
```

---

## Configuration

All configuration is via environment variables.

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `11434` | TCP port to listen on. |
| `BIND_ADDR` | `127.0.0.1` (loopback only) | Interface to bind on. Loopback-by-default for safety; set to `0.0.0.0` (or a specific interface) to expose, typically only behind a reverse proxy that handles TLS and auth. The container image overrides this to `0.0.0.0` because a container's loopback is unreachable from the host. |
| `GOOGLE_CLOUD_PROJECT` | — | GCP project id. **Required** for Vertex calls. |
| `GOOGLE_CLOUD_LOCATION` | — | Vertex location: `global`, `us-central1`, `us-east5`, `europe-west4`, etc. |
| `GATEWAY_AUTH_TOKEN` | _empty_ | If set, every `/api/*` **and `/v1/*`** request must carry `Authorization: Bearer <token>`. **Recommended** whenever `BIND_ADDR` is not loopback. |
| `MAX_REQUEST_MB` | `16` | Per-request body cap. Bodies larger than this return `413`. |
| `READ_HEADER_TIMEOUT_SEC` | `10` | Server `ReadHeaderTimeout`. Mitigates slowloris. |
| `IDLE_TIMEOUT_SEC` | `120` | Keep-alive idle timeout. |
| `WRITE_TIMEOUT_SEC` | `0` (disabled) | Server-level write timeout. **Leave 0** unless you front the gateway with a load balancer that enforces its own; long completions stream for many seconds and will be truncated otherwise. |
| `SHUTDOWN_TIMEOUT_SEC` | `30` | Max time to drain in-flight requests on SIGTERM. |
| `LOG_FORMAT` | `json` | `json` (structured, for aggregators) or `text` (human-readable). |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |
| `GW_TAGS_CACHE_TTL_SEC` | `60` | TTL for the in-memory `/api/tags` and `/v1/models` cache. Cline's model picker polls this endpoint aggressively; caching cuts the fan-out across nine publishers down to once per minute. Set to `0` to disable. |
| `GW_MAX_IMAGE_BYTES_PER_PART` | `10485760` (10 MiB) | Per-image size cap on the **decoded** bytes. Requests with a larger image return a 400 with a clear error pointing at this knob. Tightens an attack surface (a 100 MB inline base64 blob is ~75 MB of memory). |
| `GW_MAX_IMAGE_BYTES_PER_REQUEST` | `33554432` (32 MiB) | Aggregate cap across **all** decoded images in a single request. Trips on the cumulative total, not per-part. |

Credentials come from Google Application Default Credentials. No
gateway-specific GCP env var is needed beyond project + location.

### Operator endpoints

All four are unauthenticated by design so probes / scrapers / deploy
automation don't need a token, even when `GATEWAY_AUTH_TOKEN` is set.

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | Liveness — JSON `{status, version, uptime_seconds, go_version}`. Does **not** touch upstream services. |
| `GET /readyz` | Readiness — 200 when the Vertex client is configured; 503 with `reasons` array when it isn't. |
| `GET /version` | Build version only — `{"version":"…"}`. Convenient for deploy automation. |
| `GET /metrics` | Prometheus text exposition. Counters / histograms for requests, request duration, upstream tokens (by `kind={prompt,cached,completion}` and `model`), retries, loop-detector firings, tags-cache hit ratio, panics recovered, and compression bytes saved per stage. See [the metrics reference](#metrics) below. |
| `GET /` | Legacy plain-text liveness — kept for back-compat with older scrapers. |

### Build version

Embed a version string at build time so `/healthz`, `/version`, the
startup log line, and the `cline_vertex_gw_build_info` metric all report
something meaningful:

```bash
VERSION=$(git describe --tags --always --dirty)
go build -ldflags "-s -w -X main.version=${VERSION}" -o cline-vertex-gw .
```

The Makefile (`make build`) and Dockerfile (`--build-arg VERSION=…`)
both do this automatically.

### Metrics

`GET /metrics` exposes Prometheus text-exposition v0.0.4. The
operator-actionable metrics:

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `cline_vertex_gw_build_info` | gauge | `version` | Confirms which binary is running. |
| `cline_vertex_gw_requests_total` | counter | `route`, `status` | Per-route request volume / `done_reason` breakdown. |
| `cline_vertex_gw_request_duration_seconds` | histogram | `route` | Latency by route (buckets tuned for Vertex AI: 50ms–120s). |
| `cline_vertex_gw_upstream_tokens_total` | counter | `kind`, `model` | The whole point. `kind=cached/prompt` ratio tells you if Anthropic prompt caching is working; `kind=completion` × your unit price = spend. |
| `cline_vertex_gw_upstream_retries_total` | counter | `class` | Spikes in `class=rate-limited` mean you need to raise quota; spikes in `class=network` mean upstream instability. |
| `cline_vertex_gw_upstream_loop_detector_fired_total` | counter | — | How often the runaway detector saved you money. |
| `cline_vertex_gw_tags_cache_hits_total` / `_misses_total` | counters | — | Hit ratio for the `/api/tags` cache. Should approach 1.0 in steady state. |
| `cline_vertex_gw_panics_recovered_total` | counter | — | Any non-zero value indicates a bug — file an issue. |
| `cline_vertex_gw_compression_bytes_saved_total` | counter | `stage` | Cumulative bytes removed by each compression pipeline stage. |

### Token-cost optimization

The gateway sits between the client and Vertex, which makes it the right
place to shave token cost without changing the client.

**Prompt caching is always on** for every model whose publisher supports
it on Vertex AI. No env flag, no configuration — the gateway just does
the right thing for each upstream:

| Publisher | Caching mechanism | What the gateway does |
|---|---|---|
| **Anthropic** (Claude) | `cache_control: {"type":"ephemeral"}` markers on the system prompt + first user turn | Tagged automatically when the prefix exceeds the minimum cacheable size (~4 KB ≈ 1100 tokens). Subsequent same-prefix requests bill at ~10% of normal input rate. |
| **Google** (Gemini) | Implicit prefix caching on Vertex (≥1024-token prefixes) | Automatic from Vertex's side — no client change required. The gateway surfaces `cached_content_token_count` via the same telemetry as Anthropic. |
| **Cohere**, **Meta Llama**, **Mistral**, **AI21 Jamba**, **DeepSeek**, **Qwen**, **Nvidia** | No prompt-caching API on Vertex today | N/A — these publishers don't expose a caching primitive. The gateway's other optimizations (output caps, input trim, loop detector) still apply. |

For real Cline workloads on Anthropic this typically saves **70–90% of
input tokens** per follow-up turn in a session.

In addition to caching, the gateway runs an **in-flight prompt
compression pipeline** on every request. It executes between the client
and the per-publisher adapter, so every upstream benefits without
per-adapter changes:

```
client → ApplyOutputCaps → NormalizeWhitespace → CollapseEnvBlocks → TrimContents → DedupReplayedBlocks → publisher adapter
```

Each stage has an env-flag fast-path that returns the input untouched
when disabled; defaults are tuned to be safe-on-by-default.

| Variable | Default | Effect |
|---|---|---|
| `GW_NORMALIZE_WHITESPACE` | `on` | Strip BOMs, collapse CRLF → LF, trim trailing whitespace on each line, cap runs of 3+ blank lines at 2. Lossless for code (leading indentation preserved). Typical savings: 2–8% on tool-heavy sessions. |
| `GW_COLLAPSE_ENV_BLOCKS` | `on` | Cline emits an `<environment_details>` block (open tabs, file tree, terminals) on every user turn. The gateway preserves the LATEST one verbatim and replaces older ones with a one-line placeholder. Cline-specific; a no-op for non-Cline clients. Typical savings: 5–25% per long session. |
| `GW_COLLAPSE_ENV_MIN_BYTES` | `256` | Env blocks below this size are left alone (placeholder overhead would dominate). |
| `GW_DEDUP_REPLAY` | `on` | When the same large content block (≥ `GW_DEDUP_MIN_BYTES`) appears more than once across kept turns, second+ occurrences are replaced with a back-pointing placeholder (`[N bytes elided: identical content already shown in turn K (sha256=…)]`). The first occurrence is always preserved. Role-scoped — a user turn never points at an assistant turn. Typical savings: 20–60% on long Cline edit loops. |
| `GW_DEDUP_MIN_BYTES` | `512` | Minimum block size eligible for dedup. Smaller blocks can't recoup placeholder overhead (~80 bytes). |
| `GW_DEFAULT_MAX_OUTPUT_TOKENS` | `0` (off) | When a request omits `max_tokens` / `num_predict`, substitute this value. Bounds runaway generations from clients that leave the field empty. |
| `GW_MAX_OUTPUT_TOKENS_HARD` | `0` (off) | Per-deployment ceiling: any caller value above this is silently clamped DOWN. Enforces a cost-per-request ceiling regardless of what clients ask for. |
| `GW_MAX_INPUT_CHARS` | `0` (off) | Soft byte-budget on the combined size of all messages + system prompt. When exceeded, oldest turns are dropped first; the latest user turn is always preserved. Approximate ratio: ~3.5 chars/token, so `350000` ≈ 100k tokens. |
| `GW_LOOP_DETECTOR` | `on` | Master switch for the mid-stream runaway-output detector. When a model gets stuck emitting the same paragraph repeatedly the gateway cancels the upstream call early; the partial output already streamed is delivered with `done_reason=length`. |
| `GW_LOOP_DETECT_WINDOW` | `512` | Size, in chars, of the rolling buffer the loop detector inspects. |
| `GW_LOOP_DETECT_CHUNK` | `64` | Size of each hashed substring within the window. |
| `GW_LOOP_DETECT_THRESHOLD` | `6` | Identical-hash occurrences within the window that trigger detection (6 * 64 ≈ 384 chars of repetition). |

**Compression ordering matters.** Normalization runs first so trim and
dedup see clean sizes. Env-collapse runs before trim so collapsing big
stale snapshots frees budget for actual conversation content. Dedup runs
*after* trim so it only operates on the turns we're actually shipping
(prevents pointing at a dropped turn). Dedup runs *before* the
per-adapter prompt-caching tags so `cache_control` markers attach to the
post-dedup body.

**Observability.** Every completion log line includes
`cached_tok=<n> cached_pct=<n>%`. On warm Cline sessions against
caching-capable models, `cached_pct` should climb into the 80–95% range
after the first 1–2 turns. If it stays at 0% on Claude/Gemini, your
system prompt is below the per-publisher cacheable-size threshold.
Compression activity is logged separately at INFO level when triggered:
`[envblocks] collapsed N stale env block(s), saved ~XB`,
`[dedup] replaced N duplicate block(s), saved ~XB`,
`[trim] GW_MAX_INPUT_CHARS=… dropped N oldest turn(s)`.

In our headline benchmark (5-turn Cline conversation with a repeated 3 KB
file paste + 3 KB env block per user turn), the pipeline produced a
**64% byte-count reduction** end-to-end on top of prompt caching.

**Recommended starter config:**

```bash
# Prompt caching + the compression pipeline are automatic — nothing to set.
export GW_MAX_OUTPUT_TOKENS_HARD=8192      # cap per-response generation cost
export GW_MAX_INPUT_CHARS=700000           # ≈ 200k tokens; tune to your model's context
# loop detector and compression defaults are fine
```

**To disable a specific compressor** (debugging or A/B testing):

```bash
export GW_NORMALIZE_WHITESPACE=off
export GW_COLLAPSE_ENV_BLOCKS=off
export GW_DEDUP_REPLAY=off
```

---

## Security

This service brokers calls that **cost money** against your GCP project and
exposes your model catalog. Treat the port as sensitive.

- **Always set `GATEWAY_AUTH_TOKEN`** unless the listener is on `127.0.0.1`
  and only your own processes can reach it.
- **Always front with TLS** if exposing beyond the local host. The gateway
  itself speaks plaintext HTTP; put it behind nginx / Caddy / Cloud Run /
  an SSH tunnel.
- **Set `BIND_ADDR=127.0.0.1`** for local-only deployments — that single
  change neutralises most accidental exposure on dev machines.
- The auth check is constant-time (`crypto/subtle.ConstantTimeCompare`).
- Health endpoint `/` is intentionally **not** authenticated so probes /
  load-balancers don't need a token. It is liveness only, not readiness.

---

## API surface

### `GET /api/tags`

Returns the Ollama-shaped model list:

```json
{
  "models": [
    {
      "name": "gemini-2.0-flash",
      "model": "gemini-2.0-flash",
      "modified_at": "2026-05-20T10:00:00Z",
      "size": 0,
      "digest": "",
      "details": {
        "format": "vertex",
        "family": "gemini",
        "families": ["gemini"],
        "parameter_size": ""
      }
    }
  ]
}
```

`size` is reported as `0` because hosted models don't have a meaningful
on-disk size. Cline tolerates this.

### `POST /api/chat`

Body (Ollama shape):

```json
{
  "model": "claude-opus-4-7",
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

Streaming response is NDJSON — one JSON object per line — with
`done=false` chunks followed by a single `done=true` object carrying
timing/token metrics. Matches the real `ollama serve` shape that Cline
parses.

### `POST /api/generate`

Single-turn variant; same response shape minus the `messages` wrapping.

### `GET /v1/models`

Returns the OpenAI-shaped model list, sourced from the same Vertex discovery
as `/api/tags`:

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
      "id": "claude-opus-4-7",
      "object": "model",
      "created": 1747737600,
      "owned_by": "anthropic"
    }
  ]
}
```

### `POST /v1/chat/completions`

Body (OpenAI Chat Completions shape):

```json
{
  "model": "claude-opus-4-7",
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

`content` is accepted in either form OpenAI clients commonly send: a plain
string (`"content":"hi"`) or a parts array (`"content":[{"type":"text","text":"hi"}]`).
`image_url` parts with inline `data:` URLs ARE supported on vision-capable
models — see [Multimodal](#multimodal) below. Audio parts are not yet
supported.

Streaming response is `text/event-stream` with the standard OpenAI framing:

```
data: {"id":"chatcmpl-…","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-…","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

…

data: {"id":"chatcmpl-…","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}

data: [DONE]
```

Non-streaming responses (`"stream": false`) return a single JSON
`chat.completion` body with `choices[0].message.content` and `usage` set.

**Tool / function calling is fully supported** — see the
[Tool calling](#tool-calling) section above for the cross-publisher
translation matrix and a curl smoke-test.

**Not implemented in this iteration:**

- Audio `content` parts (`input_audio`) — accepted on the wire and
  silently skipped. Image parts work; see [Multimodal](#multimodal).
- `n > 1`, `logprobs`, `response_format`, `seed`, `logit_bias`,
  `frequency_penalty`, `presence_penalty` — accepted, ignored.

Errors use the standard OpenAI envelope:

```json
{"error":{"message":"…","type":"invalid_request_error","code":"model"}}
```

### `GET /`

Returns `Ollama Vertex Gateway is running`. Liveness only.

---

## Supported model namespaces

`ParsePublisher` accepts three forms:

| Form | Example | Routes to |
|---|---|---|
| Fully-qualified | `publishers/anthropic/models/claude-opus-4-7` | adapter inferred from path |
| Prefixed short | `anthropic/claude-opus-4-7` | adapter from prefix |
| Bare short | `claude-opus-4-7` | adapter inferred from substring (claude/llama/mistral/jamba/command/deepseek/qwen) |

Anything that doesn't match falls through to the Google (Gemini) adapter.

Currently implemented publishers: `google`, `anthropic`, `cohere`, `meta`,
`mistralai`, `ai21`, `deepseek-ai`, `qwen`, `nvidia`. Unsupported publishers
fail fast with a clear error rather than the SDK's misleading
`"is not servable in region ..."`.

---

## Development

```bash
go test ./...        # unit tests
go vet ./...
go build ./...
```

Test coverage focuses on the parts where bugs cost money or break clients:

- `provider.ParsePublisher` / `FormatModelName` / `MapRole` / `publisherEndpoint`
- `api.buildContents` / `api.buildContentsOAI` (role merging, system-prompt hoisting)
- `api.doneReason` / `api.finishReasonOAI`, `api.familyFromName`
- `api.OAIChatMessage.ContentString` (string-form and parts-form `content`)
- `api.genOptionsFromOAI` (max_tokens / max_output_tokens precedence)
- `api.writeSSEData` (SSE framing)
- `api.isRetryableError`, `api.classifyError`, `api.httpStatusForUpstreamError`
- `api.AuthMiddleware` (incl. `/v1/*` protection), `api.BodyLimitMiddleware`,
  `api.RecoverMiddleware`, `api.isProtectedPath`

The SSE parsers in `provider/anthropic.go`, `provider/openai_vertex.go`,
`provider/cohere_vertex.go` are not yet directly tested with fixtures — a
worthwhile next addition.

Tool-calling translation (both directions, all four publishers) is
covered by 29 dedicated tests in `provider/tools_test.go` and
`api/tools_test.go`. Multimodal translation (both directions, every
publisher + the vision capability gate + image dedup) is covered by 108
dedicated tests across `api/multimodal_test.go`,
`api/multimodal_handlers_test.go`, `provider/multimodal_test.go`, and
`provider/vision_test.go`. Total test count: 372 (was 264 before 0.9.0);
all pass under `make ci` (vet + race + staticcheck + govulncheck).

---

## Why two surfaces?

This gateway exposes both Ollama-shaped and OpenAI-shaped endpoints
deliberately, because the two ecosystems have different strengths:

| Capability | Ollama (`/api/*`) | OpenAI (`/v1/*`) |
|---|---|---|
| Cline's model picker auto-discovery | ✅ | ❌ (must type model id) |
| Per-request `Authorization: Bearer` header from client | ❌ (Cline's Ollama provider can't) | ✅ |
| Standard SSE streaming framing | ❌ (NDJSON instead) | ✅ |
| Used by LiteLLM, LangChain, official `openai` SDKs, Continue | ❌ | ✅ |
| Streaming `done_reason` / Ollama token counts | ✅ | n/a |
| Streaming `usage` block on final chunk | n/a | ✅ |

Both share the same upstream code path: routing across publishers
(Gemini + Claude + Llama + Mistral + …), retries, metrics, auth, panic
recovery, body-size limits. Switching dialects is just a client-side config
choice.

If you only ever call a fixed set of MaaS models (Llama, Mistral, Jamba,
DeepSeek, Qwen) you can also point your OpenAI client straight at Vertex's
[native OpenAI-compatible endpoint](https://cloud.google.com/vertex-ai/generative-ai/docs/multimodal/call-vertex-using-openai-library)
and skip this gateway entirely. Use this gateway when you want a single
endpoint that routes across Gemini + Anthropic + Cohere + MaaS by model
id, or when you want Cline's Ollama-style auto-discovery.

---

## License

MIT — see [`LICENSE`](./LICENSE).
