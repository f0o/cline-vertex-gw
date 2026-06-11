# 2c. Deep Compaction

The **Deep Compaction** stage is positioned at step `2c` of the optimization pipeline. It performs aggressive semantic compaction on "cold" historical turns (turns falling completely outside of the active/warm context window), reducing them from several kilobytes down to tiny 100-byte placeholders.

---

## 🔍 Why It Matters

Long-running agentic workloads (like resolving an extensive software codebase bug, running test suites, or drafting detailed documentation) can easily scale to **50 or 100+ message turns**.

Even with intermediate truncators and environment details collapsed, carrying 80 turns of conversation history swells prompt sizes to hundreds of thousands of tokens. This quadratically inflates costs, risks hitting model context ceilings, and degrades reasoning efficiency.

However, the model does *not* need to see full outputs for tasks completed dozens of turns ago. It merely needs a high-level semantic record of what was done.

**Deep Compaction** solves this. It splits the history into an **Active Warm Window** and a **Cold Compaction Zone**. All cold turns are aggressively compacted down to tiny, 100-byte high-density placeholders containing custom semantic summaries, letting agent workloads run indefinitely at near-flat token costs.

---

## ⚙️ How It Works

This stage is implemented in `pkg/pipeline/deepcompact.go` and executes as follows:

### 1. Identify the Cold Zone Boundary
The engine divides the history based on the `GW_DEEP_COMPACT_KEEP_TURNS` parameter (default: `12` turns):
*   **Warm Window**: The last `X` turns are left completely untouched, preserving detailed context for the active phase of work.
*   **Cold Zone**: All turns older than the warm limit are subjected to aggressive compaction.

### 2. Semantic Compaction of Cold Tool Responses
For any `FunctionResponse` part in the Cold Zone, the engine replaces the bulky stdout or text payload with a custom, model-aware semantic summary:
*   `read_file` ➔ `"[Stale tool result: read file '<filename>' successfully (file content elided for historical compaction)]"`
*   `execute_command` ➔ `"[Stale tool result: shell command completed (console stdout/stderr elided for historical compaction)]"`
*   `web_search` ➔ `"[Stale tool result: web query completed successfully (raw search results elided for historical compaction)]"`
*   `fetch_web_page` ➔ `"[Stale tool result: web page fetch completed (raw HTML elided for historical compaction)]"`

The original bulky outputs are saved to FSCache using SHA-256, and the lookup hash is appended to the summary.

### 3. Deep Truncation of Cold Text Parts
If a cold turn contains a plain text part that exceeds `GW_DEEP_COMPACT_MAX_BYTES` (default: `500` bytes), it is middle-elided using extremely tight bounds:
*   Keeps only `GW_DEEP_COMPACT_HEAD_BYTES` (default: `200` bytes) at the top and `GW_DEEP_COMPACT_TAIL_BYTES` (default: `100` bytes) at the bottom.
*   The raw text is saved to FSCache, returning a reversible lookup hash.

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_DEEP_COMPACT` — Master switch. Set to `true` or `1` to enable deep compaction. Defaults to `false` (disabled) on Balanced, and `true` on Aggressive and Extreme Squeeze.
*   `GW_DEEP_COMPACT_KEEP_TURNS` — Warm turns window size. Defaults to `12` turns on Balanced, and `8` turns on Extreme Squeeze.
*   `GW_DEEP_COMPACT_MAX_BYTES` — Only text parts larger than this are compacted. Defaults to `500` bytes.
*   `GW_DEEP_COMPACT_HEAD_BYTES` — Head bytes kept in cold truncated text blocks. Defaults to `200` bytes.
*   `GW_DEEP_COMPACT_TAIL_BYTES` — Tail bytes kept in cold truncated text blocks. Defaults to `100` bytes.

---

## 📝 Example

### Context Setup
*   **Keep Window**: 8 turns.
*   **Cold Turn 4**: Model executed `execute_command(command="npm run build")` which returned 20,000 characters of compiler stdout.

### Before Optimization
The gateway receives the full 20,000 characters of Turn 4's compiler stdout, even though we are currently on Turn 22.

### After Optimization
Because Turn 4 falls outside of the 8-turn warm window, the engine intercepts it and rewrites the payload:
```text
[Stale tool result: shell command completed (console stdout/stderr elided for historical compaction). Retrieve full content: hash=3a8f12c...]
```
*   **Result**: Reclaims 20,000 tokens of dead weight in the history, keeping prompt sizes lean and enabling the developer session to continue smoothly.
