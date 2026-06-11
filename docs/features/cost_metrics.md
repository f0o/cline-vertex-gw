# Cost Tracking, Scraping & Billing Tiers

The gateway integrates a dynamic, self-healing **Hybrid Billing and Cost Estimation Engine** that tracks exact USD consumption across all message completions in real-time, without requiring any manual operators to maintain hardcoded price indexes.

---

## 🔍 The Hybrid Cost Engine

Vertex AI bills tokens differently based on the model family, target routing tier, and whether the tokens hit the **prompt cache**. On startup, the gateway spins up a background thread to build a 100% complete and up-to-date pricing inventory by query-merging two dynamic channels:

### 1. Dynamic Cloud Billing Catalog API
The scraper queries Google's live [Cloud Billing Catalog API](https://cloud.google.com/billing/docs/reference/catalog/rest) to fetch live SKUs associated with Vertex AI, Gemini, and Model Garden publishers.
*   **Token Resolvers**: The engine maps free-text billing SKU descriptions (e.g., `"Vertex AI Search Input Token"`) to structured keys using the `resolveSKU` parser.
*   **Count Normalization**: It reads the `usageUnit` values, dynamically multiplying `count` or standard per-token nanos up to a standardized **Rate USD per 1 Million Tokens** baseline.

### 2. Static HTML Web Scraping (Authoritative Fallback)
Because new model versions (such as Claude 3.5 Sonnet, Claude Opus 4.8, or Gemini 3.5 Flash) are billed under API SKUs that sometimes take days or weeks to populate in the official Billing API Catalog after release, the engine has a built-in stateful **zero-dependency HTML table parser**.
*   **Web Scrape Target**: It downloads the public [GCP Generative AI Pricing Document](https://cloud.google.com/gemini-enterprise-agent-platform/generative-ai/pricing).
*   **Stateful Parsing**: It parses the table rows, handles rowspan table spans, and extracts standard input, cached input, and output rates.
*   **Merger Strategy**: It merges these values, using official live Billing API SKUs as the authoritative truth, and falling back to HTML-scraped rates for missing/unreleased models.

---

## 🔀 Google Cloud Routing Tiers

Google Cloud offers three execution and pricing tiers for generative workloads, allowing developers to trade latency for massive cost savings. Clients can dynamically request a routing tier via the headers `X-Routing-Tier` or `X-Vertex-AI-Routing-Tier`:

1.  `standard` — Default PayGo. Instant capacity allocation.
2.  `priority` — Premium speed allocation. Billed under premium rates.
3.  `flex` (alias `batch` or `flex/batch`) — Latency-tolerant queues. Google delays execution for non-critical loads, **discounting input and output token costs by exactly 50%**.

### 🔗 Under-the-Hood Google Header Translation
To ensure that Vertex AI's API gateway properly routes your requests to the correct capacity pools, the gateway transparently intercepts, sanitizes, and translates these inbound client tier headers into Google's official, mandated API headers:
*   **Flex / Batch**: Translates to `X-Vertex-AI-LLM-Request-Type: shared` and `X-Vertex-AI-LLM-Shared-Request-Type: flex`. This triggers the shared, 50%-discounted capacity pool on Google Cloud.
*   **Priority**: Translates to `X-Vertex-AI-LLM-Request-Type: shared` and `X-Vertex-AI-LLM-Shared-Request-Type: priority`.
*   **Standard**: Excludes shared headers, running the request under the standard PayGo pool.

---

## 📊 Prometheus & Console Exporters

After every request completion, the gateway calculates the session cost using the formula:

$$\text{Cost}_{\text{Total}} = (\text{Input Tokens} \times \text{Rate}_{\text{Input}}) + (\text{Cached Tokens} \times \text{Rate}_{\text{Cached}}) + (\text{Output Tokens} \times \text{Rate}_{\text{Output}})$$

This breakdown is then formatted and exported via two surfaces:

### 1. Console Log Breakdown
An indented structured summary block is printed beneath each completed streaming or non-streaming connection line:
```text
2026-06-11 07:44:00 [INFO] [req=9b4a1c] request complete: google/gemini-2.5-pro tier=flex duration=1.2s
  └─ Billed Token Metrics (Google Cloud Billing):
      Input Tokens:  12,500  @ $0.0375 / 1M  ➔  $0.000469
      Cached Tokens: 84,000  @ $0.00375 / 1M ➔  $0.000315
      Output Tokens:  1,420  @ $0.1125 / 1M  ➔  $0.000160
      Estimated Total USD Billed: $0.000944 (Source: Dynamic Catalog API)
```

### 2. Prometheus Metric Exporter
The engine feeds a thread-safe, high-frequency Prometheus cumulative counter vec:

```text
cline_vertex_gw_estimated_cost_usd_total{kind="input",model="google/gemini-2.5-pro",tier="flex"} 0.000469
cline_vertex_gw_estimated_cost_usd_total{kind="cached",model="google/gemini-2.5-pro",tier="flex"} 0.000315
cline_vertex_gw_estimated_cost_usd_total{kind="output",model="google/gemini-2.5-pro",tier="flex"} 0.000160
```

Operators can plug this counter straight into Grafana dashboards to track live cost accumulation across teams, client users, and model families.
