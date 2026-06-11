# Configuration Reference

The gateway is entirely configured via standard environment variables. This design makes it highly compatible with containers (such as Docker and Kubernetes) and simple local scripts.

---

## 🌟 The Master Toggle: `GW_PROFILE`

Instead of requiring you to configure up to 24 individual compression parameters, the gateway groups them into **5 Progressive Optimization Profiles**. 

Expose `GW_PROFILE` with one of the following levels:

1.  `1`, `passthrough`, or `raw` — Bypasses all lossy prompt compression. Delivers the raw client history directly upstream.
2.  `2`, `gentle`, or `conservative` — Lossless whitespace and system-prompt optimizations. Keeps env blocks and history mostly intact.
3.  `3`, `balanced`, or `default` — **The recommended setting (default)**. Truncates older tool outputs, collapses stale env-blocks, and uses write action elision.
4.  `4`, `aggressive` — Enables stale tool pruning, tighter tool result bounds, and partial substring-deduplication.
5.  `5`, `extreme` or `squeeze` — Aggressively shrinks everything. Deeply compacts historical turns beyond 8 turns, enables active tool pruning, and drops context down to tight byte limits.

### Profile Baselines Matrix
The following table shows the baseline settings for each profile as implemented in `pkg/pipeline/config.go`:

| Stage / Parameter | 1. Pass-Through | 2. Gentle | 3. Balanced (Default) | 4. Aggressive | 5. Extreme Squeeze |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Normalize Whitespace** | Disabled | Enabled | Enabled | Enabled | Enabled |
| **Cache Aligner** | Disabled | Enabled | Enabled | Enabled | Enabled |
| **Loop Break Trap** | Disabled | Enabled | Enabled | Enabled | Enabled |
| **Loop Trap Nudge** | Disabled | Disabled | Enabled | Enabled | Enabled |
| **Collapse Env Blocks** | Disabled | Enabled | Enabled | Enabled | Enabled |
| **Collapse Env Min Bytes** | 256 B | 1024 B | 256 B | 128 B | 64 B |
| **Dedup Replay** | Disabled | Enabled | Enabled | Enabled | Enabled |
| **Dedup Min Bytes** | 512 B | 1024 B | 512 B | 256 B | 128 B |
| **Dedup Substring** | Disabled | Disabled | Disabled | Enabled | Enabled |
| **Dedup Substring Min Bytes**| 1024 B | 1024 B | 1024 B | 512 B | 256 B |
| **Tool Result Truncate** | Disabled | Disabled | Enabled | Enabled | Enabled |
| **Tool Result Max Bytes** | 8000 B | 8000 B | 8000 B | 4096 B | 2048 B |
| **Tool Result Head Bytes** | 2000 B | 2000 B | 2000 B | 2000 B | 1000 B |
| **Tool Result Tail Bytes** | 1000 B | 1000 B | 1000 B | 1000 B | 500 B |
| **Tool Result Retain Window**| 0 turns | 5 turns | 3 turns | 2 turns | 1 turn |
| **Prune Stale Tools** | Disabled | Disabled | Disabled | Enabled | Enabled |
| **Deep Compact Enabled** | Disabled | Disabled | Disabled | Enabled | Enabled |
| **Deep Compact Keep Turns** | 12 turns | 12 turns | 12 turns | 12 turns | 8 turns |
| **Deep Compact Max Bytes** | 500 B | 500 B | 500 B | 500 B | 250 B |
| **Deep Compact Head Bytes** | 200 B | 200 B | 200 B | 200 B | 100 B |
| **Deep Compact Tail Bytes** | 100 B | 100 B | 100 B | 100 B | 50 B |
| **Active Tool Pruning** | Disabled | Disabled | Disabled | Enabled | Enabled |
| **Active Tool Pruning Window**| 20 turns | 20 turns | 20 turns | 20 turns | 10 turns |
| **Max Input Chars** | Unset | Unset | Unset | Unset | 350,000 chars |
| **Write Action Elision** | Disabled | Disabled | Enabled | Enabled | Enabled |
| **Write Action Retain Window**| 0 turns | 5 turns | 3 turns | 2 turns | 1 turn |

---

## ⚙️ Environment Variables Reference

Individual environment variables can be exported to **override** specific profile baseline settings.

### Server & Bind Configurations
*   `PORT` — Port for the HTTP server to listen on. Defaults to `11434`.
*   `BIND_ADDR` — The interface IP to bind. Defaults to `0.0.0.0` (all interfaces). For local development, set to `127.0.0.1` for safety.
*   `GATEWAY_AUTH_TOKEN` — If set, activates Bearer authentication on all `/api/*` and `/v1/*` endpoints. Requests must include the header `Authorization: Bearer <token>`.
*   `MAX_REQUEST_MB` — Maximum allowed incoming request body size in Megabytes. Defaults to `100`.
*   `READ_HEADER_TIMEOUT_SEC` — Timeout limit for reading request headers. Defaults to `10`.
*   `IDLE_TIMEOUT_SEC` — Connection idle timeout. Defaults to `120`.
*   `WRITE_TIMEOUT_SEC` — Connection write timeout. Defaults to `0` (disabled) to preserve long-running streams.
*   `SHUTDOWN_TIMEOUT_SEC` — Timeout margin for graceful server shutdown and active request draining. Defaults to `15`.

### GCP Project Parameters
*   `GOOGLE_CLOUD_PROJECT` — **Required**. Your Google Cloud Platform project ID.
*   `GOOGLE_CLOUD_LOCATION` — Your target GCP region (e.g. `us-central1` or `europe-west9`). Defaults to `us-central1`.

### Prompt Optimization Toggles
*   `GW_NORMALIZE_WHITESPACE` — Enables stripping of BOMs, carriage returns, trailing space, and multi-line runs.
*   `GW_CACHE_ALIGNER` — Enables system-prompt prefix cache stabilization by relocating volatile runtime metadata (session IDs, timestamps, working directory) to the system instructions suffix.
*   `GW_BREAK_LOOP_TRAP` — Enables LLM loop-trap deadlock resolution by deduplicating scoldings and empty turns.
*   `GW_LOOP_TRAP_NUDGE` — Appends helpful tool-use nudges to user scoldings.
*   `GW_COLLAPSE_ENV_BLOCKS` — Enables collapsing of stale `<environment_details>` snapshots.
*   `GW_COLLAPSE_ENV_MIN_BYTES` (alias `GW_COLLAPSE_ENV_THRESHOLD`) — Size threshold for env-block collapsing.
*   `GW_DEDUP_REPLAY` — Enables multi-turn text and image SHA-256 backward replay deduplication.
*   `GW_DEDUP_MIN_BYTES` (alias `GW_DEDUP_THRESHOLD`) — Size threshold for block deduplication.
*   `GW_DEDUP_SUBSTRING` — Enables partial contiguous substring replay deduplication.
*   `GW_DEDUP_SUBSTRING_MIN_BYTES` (alias `GW_DEDUP_SUBSTRING_THRESHOLD`) — Size threshold for substring deduplication.
*   `GW_TOOL_RESULT_TRUNCATE` — Enables Dual-Window progressive observation masking on older tool output responses.
*   `GW_TOOL_RESULT_MAX_BYTES` (alias `GW_TOOL_TRUNCATE_LIMIT`) — Size threshold for tool-result truncation.
*   `GW_TOOL_RESULT_HEAD_BYTES` — Head bytes kept in truncated tool outputs.
*   `GW_TOOL_RESULT_TAIL_BYTES` — Tail bytes kept in truncated tool outputs.
*   `GW_TOOL_RESULT_RETAIN_WINDOW` — Turns to keep completely unelided in both directions from the latest turn.
*   `GW_PRUNE_STALE_TOOLS` — Enables robust part-level pruning of superseded read-only tool exchanges.
*   `GW_DEEP_COMPACT` — Enables semantic deep-turn compaction of cold historical turns.
*   `GW_DEEP_COMPACT_KEEP_TURNS` — Warm turns window size.
*   `GW_DEEP_COMPACT_MAX_BYTES` — Size threshold for deep compaction.
*   `GW_DEEP_COMPACT_HEAD_BYTES` — Head bytes preserved on deep-compacted text blocks.
*   `GW_DEEP_COMPACT_TAIL_BYTES` — Tail bytes preserved on deep-compacted text blocks.
*   `GW_ACTIVE_TOOL_PRUNING` — Enables dynamic pruning of cold auxiliary tools.
*   `GW_ACTIVE_TOOL_PRUNING_WINDOW` — Sliding turn window size for monitoring tool activity.
*   `GW_ACTIVE_TOOL_PRUNING_WHITELIST` — Comma-separated list of immune tools that are never pruned.
*   `GW_MAX_INPUT_CHARS` — Soft context budget character ceiling. Triggered sliding history trim when exceeded.
*   `GW_WRITE_ACTION_ELISION` — Enables elision of historical `write_to_file` and `replace_in_file` payload bodies.
*   `GW_WRITE_ACTION_RETAIN_WINDOW` — Turn window to keep file write parameters completely unelided.

### Image & Media Sizes
*   `GW_MAX_IMAGE_BYTES_PER_PART` — Max decoded size of a single image part. Defaults to `10485760` (10 MiB).
*   `GW_MAX_IMAGE_BYTES_PER_REQUEST` — Cumulative decoded size of all images in a request. Defaults to `33554432` (32 MiB).
*   `GW_MAX_MEDIA_BYTES_PER_PART` — Max decoded size of a single audio/video/PDF part. Defaults to `52428800` (50 MiB).
*   `GW_MAX_MEDIA_BYTES_PER_REQUEST` — Cumulative decoded size of all media in a request. Defaults to `104857600` (100 MiB).

### Caching Toggles & Cache Directories
*   `GW_PROMPT_CACHE` — Master toggle for explicit/implicit prompt caching. Defaults to `true`.
*   `GW_CACHE_DIR` — Custom path for on-disk file caching. Defaults to `os.UserCacheDir()/cline-vertex-gw`.
*   `GW_FS_CACHE_TTL_SEC` — Staleness window for cached model lists and pricing. Defaults to `86400` (1 day).
*   `GW_ELIDED_TTL_SEC` — Freshness TTL for elided FSCache files. Defaults to `10800` (3 hours).
*   `GW_ELIDED_CLEANUP_INTERVAL_SEC` — Background scanner thread interval for pruning stale elided files. Defaults to `600` (10 minutes).
*   `GW_GEMINI_CACHE_TTL` — In-memory Gemini cached content lease duration. Defaults to `600` (10 minutes).
*   `GW_GEMINI_CACHE_MIN_BYTES` — Prefix size threshold for minting custom Vertex cached resources. Defaults to `128000`.

### Cost Scraper & Debugs
*   `GW_PRICING` — Set to `off` to disable all runtime pricing scrapes, cost tracking, and cost estimations. Defaults to `on`.
*   `GW_PRICING_CACHE_TTL_SEC` — Pricing rate card cache TTL. Defaults to `21600` (6 hours).
*   `GW_PRICING_DEBUG` — Set to `true` to print dynamic SKU matching, resolution pathways, and final rate cards to logs.
*   `GW_DUMP_PAYLOADS` — Set to `true` to dump complete request and response JSON payloads to output logs for debugging.
*   `LOG_LEVEL` — Minimum severity level to print (`debug`, `info`, `warn`, `error`). Defaults to `info`.
