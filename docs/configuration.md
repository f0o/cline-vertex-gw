# Configuration

All configuration for `cline-vertex-gw` is done via environment variables.

## Global Environment Variables

The following table lists all available configuration environment variables, their defaults, and their purpose:

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `11434` | TCP port to listen on. |
| `BIND_ADDR` | `127.0.0.1` (loopback only) | Interface to bind on. Loopback-by-default for safety; set to `0.0.0.0` (or a specific interface) to expose, typically only behind a reverse proxy that handles TLS and auth. The container image overrides this to `0.0.0.0` because a container's loopback is unreachable from the host. |
| `GOOGLE_CLOUD_PROJECT` | — | GCP project ID. **Required** for Vertex AI API calls. |
| `GOOGLE_CLOUD_LOCATION` | — | Vertex AI location region (e.g., `us-central1`, `us-east5`, `europe-west4`, etc.). |
| `GATEWAY_AUTH_TOKEN` | _empty_ | If set, every `/api/*` and `/v1/*` request must carry `Authorization: Bearer <token>`. **Recommended** whenever `BIND_ADDR` is not loopback. |
| `MAX_REQUEST_MB` | `16` | Per-request body cap. Bodies larger than this return `413 Request Entity Too Large`. |
| `READ_HEADER_TIMEOUT_SEC` | `10` | Server `ReadHeaderTimeout` in seconds. Mitigates slowloris attacks. |
| `IDLE_TIMEOUT_SEC` | `120` | Keep-alive idle timeout in seconds. |
| `WRITE_TIMEOUT_SEC` | `0` (disabled) | Server-level write timeout in seconds. **Leave 0** unless you front the gateway with a load balancer that enforces its own; long completions stream for many seconds and will be truncated otherwise. |
| `SHUTDOWN_TIMEOUT_SEC` | `30` | Max time in seconds to drain in-flight requests on SIGTERM. |
| `LOG_FORMAT` | `json` | `json` (structured, for aggregators) or `text` (human-readable). |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |
| `GW_TAGS_CACHE_TTL_SEC` | `60` | TTL in seconds for the in-memory `/api/tags` and `/v1/models` cache. Cline's model picker polls this endpoint aggressively; caching cuts the fan-out across multiple publishers down to once per minute. Set to `0` to disable. |
| `GW_MAX_MEDIA_BYTES_PER_PART` | `10485760` (10 MiB) | Per-media (images, audio, video, PDF) size cap on the **decoded** bytes. Requests with a larger file return a `400 Bad Request`. |
| `GW_MAX_MEDIA_BYTES_PER_REQUEST` | `33554432` (32 MiB) | Aggregate cap across **all** decoded media in a single request. |
| `GW_PRICING` | `on` | Cost estimation toggle. When on, the gateway scrapes per-token prices live from the GCP **Cloud Billing Catalog API** and prints a per-request USD breakdown alongside the request stats (and feeds the `cline_vertex_gw_estimated_cost_usd_total` metric). Set to `off`/`false`/`0` to disable all pricing scrapes and cost output. |
| `GW_PRICING_CACHE_TTL_SEC` | `21600` (6h) | Refresh interval in seconds for the live pricing table. Prices change on the order of months, so the catalog is scraped at most once per interval (warmed once at startup, then refreshed lazily in the background on request completion). |
| `GW_PRICING_DEBUG` | `off` | When on, the pricing scrape logs a verbose diagnostic dump (`[pricing][debug]`): the matched/not-matched billing services, a per-SKU resolution trace, and the final per-model resolved rate table. Use it to verify or troubleshoot the SKU→model/rate mapping against your project's catalog. |
| `GW_PRICING_TIER` | `low` | Overrides or sets specific pricing tiers for models when using custom billing profiles or MaaS. Set to `low` (default), `medium`, or `high`. |
| `GW_GEMINI_SEARCH_GROUNDING` | _empty_ | Enable native Vertex Google Search Grounding / WebSearch for Gemini models. Set to `google_search` (standard Google Search) or `enterprise_web_search` (requires a Vertex AI Search data store setup) to enable globally. Also triggered dynamically per-request if the client passes a tool/function named `web_search` or `google_search`. |
| `GW_GEMINI_SEARCH_THRESHOLD` | `-1.0` (disabled) | Optional dynamic search triggering threshold (float, e.g. `0.5`). Lower values trigger search more conservatively when the model lacks confidence; `-1.0` uses Google's default. Only evaluated when Google Search Grounding is active. |

---

## Authentication & Credentials

The gateway relies on standard **Google Application Default Credentials (ADC)**. No gateway-specific GCP environment variables or configuration files are needed beyond the core `GOOGLE_CLOUD_PROJECT` and `GOOGLE_CLOUD_LOCATION` settings.

The authentication is resolved in order:
1. Credentials pointed to by the `GOOGLE_APPLICATION_CREDENTIALS` environment variable (JSON file path).
2. Credentials provided by the local developer environment via `gcloud auth application-default login`.
3. The default service account attached to the Google Cloud resource (Cloud Run, GKE, GCE) when deployed in GCP.

---

## Operator & Administrative Endpoints

The gateway exposes five operator-facing endpoints. All five endpoints are **unauthenticated by design**, meaning health-checks, liveness probes, and metrics scrapers do not need to provide a bearer token even if `GATEWAY_AUTH_TOKEN` is set.

| Endpoint | Method | Purpose | Response Format |
|---|---|---|---|
| `/healthz` | `GET` | Liveness check. Returns gateway status, version, uptime, and Go runtime version. Does not touch upstream services. | JSON |
| `/readyz` | `GET` | Readiness check. Returns `200 OK` when the Vertex client is fully configured; `503 Service Unavailable` with a `reasons` array if it is not. | JSON |
| `/version` | `GET` | Retrieves the gateway's build version string. Useful for automated deployments and checking release drift. | JSON |
| `/metrics` | `GET` | Exposes standard Prometheus text-exposition (v0.0.4) metrics. Includes counters, histograms, and gauges for request count, latency, tokens, cost, and compression statistics. | Plain Text |
| `/` | `GET` | Legacy plain-text liveness response. Maintained for backward compatibility with older scrapers and load balancers. | Plain Text |

---

## Build Version Injection

To make the `/healthz`, `/version`, the startup log line, and the `cline_vertex_gw_build_info` metric report meaningful version strings, inject the version at build time using Go linker flags:

```bash
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
go build -ldflags "-s -w -X main.version=${VERSION}" -o cline-vertex-gw .
```

The provided `Makefile` (`make build`) and `Dockerfile` (`--build-arg VERSION=...`) handle this injection automatically.
