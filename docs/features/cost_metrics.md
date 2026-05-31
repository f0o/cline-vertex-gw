# Metrics & Cost Tracking

`cline-vertex-gw` includes built-in systems for tracking, estimating, and exporting API usage, cost, and optimization performance.

---

## 1. Prometheus Metrics

The gateway exposes a Prometheus-compatible metrics endpoint at `GET /metrics` (using standard text-exposition v0.0.4). This endpoint is unauthenticated by design to allow Prometheus scrapers and monitoring agents to pull metrics easily.

### Available Metrics

The following metrics are exported by the gateway:

| Metric | Type | Labels | Description |
|---|---|---|---|
| `cline_vertex_gw_build_info` | Gauge | `version` | Confirms which binary version is currently running. |
| `cline_vertex_gw_requests_total` | Counter | `route`, `status` | Total volume of HTTP requests received, broken down by route path and response status code. |
| `cline_vertex_gw_request_duration_seconds` | Histogram | `route` | Response latency by route in seconds. Buckets are custom-tuned for LLM stream characteristics: `[0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 30, 45, 60, 90, 120]`. |
| `cline_vertex_gw_upstream_tokens_total` | Counter | `kind`, `model` | Total token count billed by upstream models. `kind` is one of: `prompt` (standard input), `cached` (prompt cached input), or `completion` (generation output). Comparing `cached` vs `prompt` helps evaluate prompt cache effectiveness. |
| `cline_vertex_gw_upstream_retries_total` | Counter | `class` | Spikes in `class=rate-limited` indicate that you are hitting Vertex AI quota caps; spikes in `class=network` indicate upstream service instability. |
| `cline_vertex_gw_upstream_loop_detector_fired_total` | Counter | — | Count of times the gateway runaway loop-detector successfully intercepted an infinite tool-calling loop and terminated the request to save money. |
| `cline_vertex_gw_tags_cache_hits_total` <br> `cline_vertex_gw_tags_cache_misses_total` | Counter | — | In-memory cache statistics for `/api/tags` and `/v1/models` requests. The hit ratio should approach `1.0` in steady state. |
| `cline_vertex_gw_panics_recovered_total` | Counter | — | Total number of panics gracefully recovered by server middleware. Any value greater than zero indicates a bug. |
| `cline_vertex_gw_compression_bytes_saved_total` | Counter | `stage` | Cumulative count of raw text bytes removed from messages by individual optimization/compression pipeline stages: `envblocks`, `toolresult`, `dedup`, `dedup_substring`, `normalize`, `prune_tools`, `trim`, `loopbreak`, `deepcompact`, `active_tool_pruning`. |
| `cline_vertex_gw_estimated_cost_usd_total` | Counter | `kind`, `model`, `tier` | Cumulative estimated USD cost spent, broken down by token `kind` (`input`, `cached`, `output`), resolved `model`, and routing `tier` (`standard`, `priority`, `flex`). Disabled when `GW_PRICING=off`. |

---

## 2. Real-Time Cost Estimation

When `GW_PRICING` is enabled (enabled by default), the gateway calculates the estimated USD cost of every request in real-time. Immediately following the request statistics in your logs, a detailed cost breakdown is printed:

```
[chat-stream req=1a2b3c-004] done total=4.2s … prompt_tok=18500 cached_tok=17000 cached_pct=92% eval_tok=820 tps=195.2 reason=stop
[chat-stream req=1a2b3c-004] cost total=$0.012063 input=$0.001875 cached=$0.005270 output=$0.004918 rates_per_mtok(in/cached/out)=$1.250/$0.310/$10.000 src="Vertex AI"
```

### How Live Price Scaping Works
To prevent hardcoded pricing tables from drifting out of date, the gateway **scrapes pricing rates live from the GCP Cloud Billing Catalog API** (`cloudbilling.googleapis.com`):
1. **Startup Warm-up & Cache:** On startup, the gateway pages through the public billing SKUs of Vertex-related services and caches them in memory.
2. **Lazily Refreshed:** The cache is refreshed in the background every `GW_PRICING_CACHE_TTL_SEC` (default: 6 hours).
3. **SKU Resolution:** The gateway matches free-text SKU descriptions to specific model identifiers and token kinds (e.g. Gemini input/output, Anthropic Claude input/output, MaaS inputs, etc.), normalizing rates to USD per 1M tokens.
4. **Differentiation:** **Cached** prompt tokens are calculated using GCP's reduced cached-input rates; remaining prompt tokens use standard input rates; completion tokens use standard output rates.

### Pricing Considerations & Caveats
- **Estimates Only:** Since SKU descriptions are free-text, models with SKUs that cannot be resolved with absolute confidence will print `cost=unavailable` and are excluded from the `cline_vertex_gw_estimated_cost_usd_total` metric to prevent incorrect reporting.
- **Approximations:** For character-priced SKUs (e.g., certain legacy vision or speech components), tokens are mathematically approximated from raw characters and the log line is tagged with `cost (approx)`.
- **Base Pricing:** The gateway models rates against the base/lowest input tier. Long-context premium tiers are not calculated.
- **API Access:** This feature requires the public GCP Cloud Billing API to be enabled and your runtime credentials to have permission to read the public catalog. If catalog API read permissions fail (e.g. `403 Forbidden` or `404 Not Found`), pricing estimation is gracefully disabled without interrupting chat or tag requests.
- **Disabling:** Set `GW_PRICING=off` to completely disable the price scraping and cost tracking systems.

---

## 3. Dynamic Routing & Pricing Tiers

Clients (including Cline, LiteLLM, or custom backend integrations) can dynamically configure the Vertex AI routing and pricing tier on a per-request basis by sending custom HTTP headers. If no routing tier header is supplied, the gateway defaults to the standard Vertex AI routing tier.

The gateway inspects incoming requests for the following headers in order of precedence:
1. `X-Routing-Tier`
2. `X-Vertex-AI-Routing-Tier`

The header values are parsed case-insensitively and normalized:

- **`standard`** (or empty/invalid/unknown) -> Maps to Vertex AI's **Standard** routing tier.
- **`priority`** -> Maps to Vertex AI's **Priority** routing tier, offering lower latency, higher SLA, and higher quota limits for demanding workloads.
- **`flex`** (or `batch`, `flex/batch`) -> Maps to Vertex AI's **Flex/Batch** routing tier, which offers significantly discounted rates for non-urgent requests.

The gateway automatically propagates the normalized value via the outbound `X-Vertex-AI-Routing-Tier` header on all API calls made to Google's endpoints (applying to calls made through Google's Generative AI SDK, Anthropic's Messages SDK, and the shared REST clients).
