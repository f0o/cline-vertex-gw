# Prometheus Metrics & Observability

`cline-vertex-gw` places a strong emphasis on high-performance operational observability. To maintain a lightweight footprint with zero external dependencies, the gateway includes a hand-rolled Prometheus metrics engine. This engine exposes standard text-exposition (v0.0.4) metrics on `GET /metrics` without incurring the cost of heavy client libraries.

---

## 1. Hand-Rolled Exporter Architecture

To prevent dependency bloat and minimize heap allocations during telemetry scrapes, the metrics collector (`pkg/api/metrics.go`) implements specialized, thread-safe Go primitives:
- **`counterVec`:** A label-to-count map utilizing a `sync.RWMutex`. Cardinality is strictly bounded by active routes and discovered model names to prevent memory growth attacks.
- **`floatCounterVec`:** A label-to-float64 monotonic tracker designed for fractional currency tracking, preventing floating-point precision loss over thousands of small sub-cent API calls.
- **`histogram`:** A fixed-bucket request duration tracker with custom buckets tuned specifically for typical cloud LLM streaming characteristics (TTFB and full compilation streams).

Scrapes on `GET /metrics` are permanently unauthenticated by design, enabling monitoring tools (like Prometheus, Grafana Agent, or Datadog) to scrape telemetry without configuring complex bearer authorization.

---

## 2. Active Prometheus Metrics

The following metrics are registered and exported by the gateway:

| Metric Name | Type | Labels | Description / Help |
|---|---|---|---|
| `cline_vertex_gw_build_info` | Gauge | `version` | Confirms the exact build version of the running binary. Value is always `1`. |
| `cline_vertex_gw_requests_total` | Counter | `route`, `status` | Total HTTP requests handled by the gateway. `route` tracks active endpoints; `status` tracks normalized HTTP status codes. |
| `cline_vertex_gw_request_duration_seconds` | Histogram | `route` | HTTP request duration in seconds. Buckets are custom-tuned for typical Vertex AI latencies: `[0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120]`. |
| `cline_vertex_gw_upstream_tokens_total` | Counter | `kind`, `model` | Total tokens consumed. `kind` is one of `prompt` (billed input), `cached` (prompt-cached input), or `completion` (generated output). Comparing `cached` vs `prompt` tracks KV cache efficiency. |
| `cline_vertex_gw_upstream_retries_total` | Counter | `class` | Cumulative upstream retry events. `class` indicates the error type: `rate-limited` (HTTP 429/503 quota limits) or `network` (TCP disconnects and timeouts). |
| `cline_vertex_gw_upstream_loop_detector_fired_total` | Counter | — | Total count of times the runaway loop-detector successfully intercepted an infinite agent tool-calling loop and truncated the stream. |
| `cline_vertex_gw_tags_cache_hits_total` | Counter | — | Discovery tag requests (Ollama `/api/tags` and OpenAI `/v1/models`) served directly from the in-memory cache. |
| `cline_vertex_gw_tags_cache_misses_total` | Counter | — | Discovery tag requests that missed cache and fanned out queries to upstream Vertex AI. |
| `cline_vertex_gw_panics_recovered_total` | Counter | — | Panics caught and handled gracefully by the server's panic-recovery middleware. A value greater than zero indicates a bug. |
| `cline_vertex_gw_compression_bytes_saved_total` | Counter | `stage` | Cumulative count of raw text bytes removed from messages by individual optimization pipeline stages: `envblocks`, `toolresult`, `dedup`, `dedup_substring`, `normalize`, `prune_tools`, `trim`, `loopbreak`, `deepcompact`, `active_tool_pruning`. |
| `cline_vertex_gw_estimated_cost_usd_total` | Counter | `kind`, `model`, `tier` | Cumulative estimated USD cost spent, broken down by token `kind` (`input`, `cached`, `output`), resolved `model`, and routing `tier` (`standard`, `priority`, `flex`). Disabled when `GW_PRICING=off`. |

---

## 3. Real-Time Telemetry Logging

At the end of every chat or generation request, the gateway prints a structured, high-fidelity log line summarizing operational telemetry:

```
2026-06-10T12:42:00.000Z INFO request done phase=chat-stream total=4.2s load=450ms eval=3.75s prompt_tok=18500 cached_tok=17000 cached_pct=91.89 eval_tok=820 tps=218.66 reason=stop
```

### Telemetry Parameters Explained
- **`total`:** The overall wall-clock time from receiving the HTTP request to flushing the final bytes of the completion.
- **`load`:** Approximate connection and setup latency (Time to First Byte / TTFB).
- **`eval`:** Pure streaming duration (from the first token flushed to the terminal block).
- **`prompt_tok`:** Total prompt tokens submitted.
- **`cached_tok`:** Subset of `prompt_tok` that were served from the upstream prompt cache.
- **`cached_pct`:** Share of the prompt served from cache (e.g. `91.89%` representing extreme savings).
- **`eval_tok`:** Number of tokens generated in the completion response.
- **`tps`:** Generation speed in Tokens Per Second.
- **`reason`:** Normalized Ollama-style stop condition: `stop` (natural end), `length` (context boundary hit), `safety` (content filter), or `recitation` (licensing guardrails).

---

## 4. Querying Metrics

To verify metrics are exposing correctly, query the `/metrics` endpoint locally:

```bash
curl -s http://127.0.0.1:11434/metrics | grep cline_vertex_gw
```

*Example Prometheus output format:*
```text
# HELP cline_vertex_gw_upstream_loop_detector_fired_total Streams terminated by the runaway/loop detector.
# TYPE cline_vertex_gw_upstream_loop_detector_fired_total counter
cline_vertex_gw_upstream_loop_detector_fired_total 0

# HELP cline_vertex_gw_tags_cache_hits_total /api/tags requests served from the in-memory cache.
# TYPE cline_vertex_gw_tags_cache_hits_total counter
cline_vertex_gw_tags_cache_hits_total 42

# HELP cline_vertex_gw_compression_bytes_saved_total Cumulative bytes removed by each compression pipeline stage.
# TYPE cline_vertex_gw_compression_bytes_saved_total counter
cline_vertex_gw_compression_bytes_saved_total{stage="normalize"} 14850
cline_vertex_gw_compression_bytes_saved_total{stage="envblocks"} 38200
```
