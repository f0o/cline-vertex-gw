# Configuration Manual

All configurations for `cline-vertex-gw` are managed through environment variables. The gateway does not require any static configuration files.

---

## Server Environment Variables

The following table lists all general server configuration variables:

| Variable | Default Value | Purpose / Constraint |
|---|:---:|---|
| `PORT` | `11434` | TCP port to listen on. |
| `BIND_ADDR` | `127.0.0.1` | Network interface to bind. Defaults to local loopback only for safety. Set to `0.0.0.0` in Docker. |
| `GOOGLE_CLOUD_PROJECT` | _required_ | Google Cloud Project ID. |
| `GOOGLE_CLOUD_LOCATION` | _required_ | Vertex AI location region (e.g., `us-central1`, `us-east5`, `europe-west4`, etc.). |
| `GATEWAY_AUTH_TOKEN` | _empty_ | If set, every API request must carry a matching `Authorization: Bearer <token>` header. |
| `MAX_REQUEST_MB` | `16` | Per-request body cap (in MiB). Requests larger than this return `413 Request Entity Too Large`. |
| `READ_HEADER_TIMEOUT_SEC` | `10` | Server `ReadHeaderTimeout` in seconds, protecting against Slowloris attacks. |
| `IDLE_TIMEOUT_SEC` | `120` | Keep-alive idle socket timeout in seconds. |
| `WRITE_TIMEOUT_SEC` | `0` | Server-level write timeout in seconds. **Leave at 0** for streaming endpoints. |
| `SHUTDOWN_TIMEOUT_SEC` | `30` | Max time (seconds) to wait for draining active requests upon SIGTERM. |
| `LOG_LEVEL` | `info` | Logging verbosity: `debug` \| `info` \| `warn` \| `error`. |
| `LOG_FORMAT` | `json` | Logging format layout: `json` (structured logs) \| `text` (human-readable). |
| `GW_TAGS_CACHE_TTL_SEC` | `60` | TTL in seconds for the model discovery tags cache to optimize aggressive picker polling. |

---

## Google Cloud Authentication

The gateway utilizes Google's standard **Application Default Credentials (ADC)**. No custom credentials or secrets files are configured inside the gateway. The runtime resolves ADC in the following order:

1. **`GOOGLE_APPLICATION_CREDENTIALS`:** Environment variable pointing to a JSON service account key file.
2. **`gcloud` Authenticated User:** Local development session authenticated via `gcloud auth application-default login`.
3. **Attached Service Account:** The default service account attached to the Google Cloud hosting resource (e.g. Cloud Run, GKE, GCE) when deployed in GCP.

---

## Token-Cost Optimization Profiles (`GW_PROFILE`)

The gateway provides **5 progressive optimization levels** to compress context windows and maximize cache efficiency. Setting the `GW_PROFILE` environment variable applies a curated set of baseline parameters. Individual `GW_*` environment variables can still be set alongside `GW_PROFILE` to explicitly override parameters.

### Supported Profiles

1. **`passthrough` / `raw` / `1`:** Turns off all prompt modifications. Raw input is sent upstream.
2. **`gentle` / `conservative` / `2`:** Activates non-destructive optimizations like whitespace normalization and cache alignment.
3. **`balanced` / `default` / `3` (Default):** Evaluates a balanced baseline of context reduction and protection loops.
4. **`aggressive` / `4`:** Actively compresses history, collapses environment blocks, and trims read-only tool cycles.
5. **`extreme` / `squeeze` / `5`:** Maximizes prompt shrinkage with small truncation limits and aggressive active tool pruning.

### Detailed Optimization Parameters

| Parameter / Knob | Standard Default | Fallback Env Alias | Gentle (2) | Balanced (3) | Aggressive (4) | Extreme (5) |
|---|:---:|---|:---:|:---:|:---:|:---:|
| `GW_NORMALIZE_WHITESPACE` | `on` | — | `on` | `on` | `on` | `on` |
| `GW_CACHE_ALIGNER` | `on` | — | `on` | `on` | `on` | `on` |
| `GW_BREAK_LOOP_TRAP` | `on` | — | `on` | `on` | `on` | `on` |
| `GW_LOOP_TRAP_NUDGE` | `on` | — | `off` | `on` | `on` | `on` |
| `GW_COLLAPSE_ENV_BLOCKS` | `on` | — | `on` | `on` | `on` | `on` |
| `GW_COLLAPSE_ENV_MIN_BYTES` | `256` | `GW_COLLAPSE_ENV_THRESHOLD` | `1024` | `256` | `128` | `64` |
| `GW_DEDUP_REPLAY` | `on` | — | `on` | `on` | `on` | `on` |
| `GW_DEDUP_MIN_BYTES` | `512` | `GW_DEDUP_THRESHOLD` | `1024` | `512` | `256` | `128` |
| `GW_DEDUP_SUBSTRING` | `off` | — | `off` | `off` | `on` | `on` |
| `GW_DEDUP_SUBSTRING_MIN_BYTES`| `1024` | `GW_DEDUP_SUBSTRING_THRESHOLD`| `1024` | `1024` | `512` | `256` |
| `GW_TOOL_RESULT_TRUNCATE` | `on` | — | `off` | `on` | `on` | `on` |
| `GW_TOOL_RESULT_MAX_BYTES` | `8000` | `GW_TOOL_TRUNCATE_LIMIT` | `8000` | `8000` | `4096` | `2048` |
| `GW_TOOL_RESULT_RETAIN_WINDOW` | `3` | — | `5` | `3` | `2` | `1` |
| `GW_WRITE_ACTION_ELISION` | `on` | — | `off` | `on` | `on` | `on` |
| `GW_PRUNE_STALE_TOOLS` | `off` | — | `off` | `off` | `on` | `on` |
| `GW_DEEP_COMPACT` | `off` | — | `off` | `off` | `on` | `on` |
| `GW_DEEP_COMPACT_KEEP_TURNS` | `12` | — | `12` | `12` | `12` | `8` |
| `GW_DEEP_COMPACT_MAX_BYTES` | `500` | — | `500` | `500` | `500` | `250` |
| `GW_ACTIVE_TOOL_PRUNING` | `off` | — | `off` | `off` | `on` | `on` |
| `GW_ACTIVE_TOOL_PRUNING_WINDOW`| `20` | — | `20` | `20` | `20` | `10` |
| `GW_MAX_INPUT_CHARS` | `0` (unlim) | — | `0` | `0` | `0` | `350000` |

---

## Live Cost Estimation & Billing Settings

To track costs, the gateway resolves prices directly from the public Google Cloud Billing catalog on startup.

| Variable | Default Value | Purpose / Constraints |
|---|:---:|---|
| `GW_PRICING` | `on` | Cost estimation toggle. Set to `off` to disable price catalog scrapes and logs. |
| `GW_PRICING_CACHE_TTL_SEC` | `21600` (6h) | Interval in seconds to lazily refresh cached pricing rates from Google Billing Catalog. |
| `GW_PRICING_DEBUG` | `off` | When set to `on`, outputs extremely verbose trace logs during startup SKU resolution. |

---

## Google Search Grounding Settings

Enables native web-search grounding over Google Search for Gemini models:

| Variable | Default Value | Purpose / Constraints |
|---|:---:|---|
| `GW_GEMINI_SEARCH_GROUNDING` | _empty_ | Set to `google_search` or `enterprise_web_search` to force search on Gemini queries. |
| `GW_GEMINI_SEARCH_THRESHOLD` | `-1.0` (disabled) | Dynamic threshold (float, e.g., `0.5`) to trigger search when confidence drops. |

---

## Administrative Endpoints

The gateway exposes five operator-facing endpoints. These endpoints are **permanently unauthenticated** by design to simplify health probes, deploy automations, and metrics pulling.

| Endpoint | Method | Purpose / Response Shape |
|---|:---:|---|
| `/healthz` | `GET` | Return JSON detailing gateway liveness, version, runtime details, and uptime. |
| `/readyz` | `GET` | Return `200 OK` JSON when Vertex AI client is ready; `503 Service Unavailable` with details otherwise. |
| `/version` | `GET` | Retrieves the gateway's build version string. |
| `/metrics` | `GET` | Exposes standard Prometheus text-exposition metrics for log/monitoring aggregators. |
| `/` | `GET` | Legacy raw plain-text liveness response (`"cline-vertex-gw running..."`). |

---

## Build Version Injection

To make version reporting across `/healthz`, `/version`, and metrics meaningful, inject the Git version string at compile time using Go linker flags:

```bash
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
go build -ldflags "-s -w -X main.version=${VERSION}" -o cline-vertex-gw .
```
The included `Makefile` and standard container `Dockerfile` run this command automatically.
