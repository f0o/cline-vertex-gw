# Lossless Retrieve (CCR Loop)

The **Compress-Cache-Retrieve (CCR)** architecture is the gateway's core infrastructure for making lossy prompt optimizations (like tool-result truncation or file-write elisions) completely **lossless, reversible, and safe**.

---

## 🔍 Why It Matters

When developer agents run extensive tasks, compressing history is vital to stay within budget and prevent runaway costs. However, aggressively stripping text (like eliding a file modification or a long compiler output) risks removing details the model genuinely needs to reference several turns later.

Standard solutions like "summarizing history" are slow, expensive, and can hallucinate details.

The **CCR Loop** solves this. When the gateway strips an older text block, it compiles it as a lossless file to a local filesystem cache, replacing it with a small marker containing its SHA-256 hash. If the model decides it needs to see the original content, it is given a specialized retrieval tool to **recall the raw unelided data instantly with exactly zero network overhead**.

---

## ⚙️ How It Works

The CCR Loop is implemented across `pkg/pipeline/ccr_helpers.go`, `pkg/cache/fs_cache.go`, and the streaming handler loops in `pkg/api/handlers.go`. It executes across four synchronized phases:

```
[Outbound Turn] ──► 1. HASH & CACHE ──► Writes raw text to ~/.cache/cline-vertex-gw/elided_<hash>.json
                           │
                           ▼
                    2. INJECT TOOL  ──► Scans context for tombstone markers and appends
                           │            retrieve_elided_content tool to allowable set
                           ▼
                    3. MODEL CALLS  ──► Model invokes retrieve_elided_content(hash="...")
                           │
                           ▼
                    4. INTERCEPT    ──► Gateway intercepts call locally, reads file from FSCache,
                                        injects raw text back to turn, and restarts stream
```

### 1. Hash & Cache (The `SaveToElidedCache` Stage)
When a compressor (like `TruncateToolResults` or `ElideHistoricalWriteActions`) decides to prune a text part:
*   It hashes the string using SHA-256 and writes it as an atomic JSON file to the local cache directory (`~/.cache/cline-vertex-gw/elided_<hash>.json`).
*   It substitutes the text with a short marker:
    `"... Retrieve full content: hash=e5a2f8c..."`

### 2. Dynamic Tool Injection
Before sending the request upstream, `InjectRetrieveElidedContentTool` scans all non-nil conversation turns.
*   If it finds any text containing the string `"Retrieve full content: hash="`, it dynamically appends the `retrieve_elided_content` tool declaration to the request's allowed tool list.
*   **Why**: If no placeholders exist in the history, the tool is **not** injected. This prevents bloating the active toolset unnecessarily.

### 3. Local Interception & Zero-Network Resolving
If the model calls `retrieve_elided_content(hash="e5a2f8c...")` to recall code:
*   The gateway's HTTP handlers (`handlers.go`) intercept this call **locally inside the stream-loop**.
*   It reads the corresponding JSON file directly from the local FSCache.
*   It injects the raw unelided content back into the conversation, replacing the placeholder.
*   It **restarts the stream pipeline internally**.
*   **The Client Wins**: The client never sees this transaction. No network round-trips to Google are wasted, and the retrieval resolves at local disk speed (~1ms).

### 4. The Recursive Elision Safeguard
When the gateway restarts the stream with the retrieved raw content in the history, the compression pipeline runs again. 
*   **The Danger**: Because the retrieved content is older than the retain window, the pipeline would immediately elide/truncate it back to a placeholder, trapping the model in an infinite recursive loop.
*   **The Fix**: The engine includes `HasRetrievalToolCall(contents)`. If any `retrieve_elided_content` tool call or response is active in the history, the gateway **completely bypasses all lossy compression stages** (`TruncateToolResults`, `ElideHistoricalWriteActions`, `DeepCompactHistoricalTurns`, and `PruneStaleTools`). This guarantees raw text successfully reaches the model, resolving files with 100% correctness.

---

## 🧹 Automated Cache Housekeeping

To prevent elided files from consuming excessive disk space over time, the cache includes an automated background cleaning routine:

### 1. High-Performance Metadata Check
Determining cache age avoids the CPU and memory overhead of unmarshaling JSON envelopes. The scanner reads the native filesystem modification time (`FileInfo.ModTime()`) to evaluate file age. Since cache files are written once atomically, the filesystem's `mtime` is guaranteed to be 100% accurate.

### 2. Periodic Ticker Loop
A background goroutine in `cmd/cline-vertex-gw/main.go` runs periodically based on `GW_ELIDED_CLEANUP_INTERVAL_SEC` (default: `10 minutes`):
*   It scans the cache folder.
*   Any `elided_*.json` file older than `GW_ELIDED_TTL_SEC` (default: `3 hours`) is deleted.
*   An initial cleanup is safely scheduled 5 seconds after server startup to immediately reclaim space from previous sessions.

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_CACHE_DIR` — Custom path for on-disk file caching. Defaults to `os.UserCacheDir()/cline-vertex-gw`.
*   `GW_ELIDED_TTL_SEC` — Cache file age threshold before deletion. Defaults to `10800` seconds (3 hours) to optimize disk space.
*   `GW_ELIDED_CLEANUP_INTERVAL_SEC` — Background ticker interval. Defaults to `600` seconds (10 minutes).
