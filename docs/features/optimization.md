# Token-Cost Optimization

`cline-vertex-gw` sits between your client and Vertex AI, providing an intelligent **Token-Cost Optimization Pipeline** to dramatically shrink your context footprint and reduce API costs.

In a benchmark conversation (a 5-turn Cline session with a repeated 3 KB file paste + a 3 KB environment block on every user turn), this pipeline produces an end-to-end **64% input byte-count reduction** on top of Google/Anthropic's native prompt caching.

---

## 1. The Optimization Pipeline

The pipeline runs sequentially on inbound messages before they are dispatched to the upstream publisher. It consists of several distinct stages:

### White-Space & Comment Normalization (`normalize`)
Reduces redundant spacing, carriage returns, trailing empty lines, and standard format comments in code/text blocks. This clean-up can save up to 5-10% of raw text space without affecting meaning or syntax.
- **Toggle:** `export GW_NORMALIZE_WHITESPACE=off` (defaults to `on`)

### Interactive Environment Block Collapsing (`envblocks`)
LLM agents (like Cline) often append massive environment variable dumps, file lists, or status blocks on *every single turn*.
- The pipeline identifies these massive, redundant blocks across your conversation history.
- It automatically collapses older, identical copies of these blocks into small 1-line pointers (e.g. `[env block collapsed, saved 3200B]`).
- **Critical Safety Guard:** The **latest user turn** is explicitly exempted from collapsing, ensuring the model always sees the exact, current system and environment state.
- **Toggle:** `export GW_COLLAPSE_ENV_BLOCKS=off` (defaults to `on`)

### Multimodal Part-Level Deduplication (`dedup`)
Agent tools often upload identical file attachments or screenshots repeatedly. This stage hashes attachments and drops duplicate assets, replacing them with standard back-references (e.g. `[image matched turn 2, saved 810KB]`).
- **Toggle:** `export GW_DEDUP_REPLAY=off` (defaults to `on`)
- **Substring Deduplication:** High-density substring-level deduplication is also available (`export GW_DEDUP_SUBSTRING=on`), but is disabled by default because it rewrites the interiors of text turns.

### Interactive Tool Pruning & Truncation (`toolresult`)
When multi-part agents are executing long-running tasks, their history accumulates massive logs from terminal runs or file content reads.
- **Stale Tool Pruning:** Automatically drops matched tool call-and-response pairs from the history if they represent read-only, idempotent operations that have been superseded by subsequent actions. It always removes the call + response together to preserve role alternation.
- **Tool-Result Truncation:** Caps extremely large tool outputs (such as massive stack traces or long directory listings) by keeping a portion of the head and tail while discarding the middle, injecting a clear truncation notice in between.
- **Safety Guard:** Truncation is evaluated before other metrics to ensure the budget sees already shrunken sizes, and it never touches the latest turn.

### Deep Historical Turn Compaction & Collapsing (`deepcompact`)
As agentic workloads run beyond 40-50 turns (and up to 100+ turns), the cumulative size of past tool outputs, large file dumps, and status logs creates massive context overhead.
- **Deep Compaction:** Targets "cold" historical turns (turns older than a sliding keep window of e.g. 12 turns).
- **Fully Semantic Collapsing (Strategy B):** Instead of generic text elisions, `deepcompact` semantically recognizes known bulky tool outputs (`read_file`, `execute_command`, `web_search`, `web_fetch`) and collapses them into concise formatted strings (e.g. `[Stale tool result: read file 'main.go' successfully (file content elided for historical compaction)]`).
- Other unrecognized tools and large user/assistant text blocks are deeply middle-elided down to tiny 100-byte placeholders.
- This preserves the high-level trajectory memory and alternation structure of the entire session at near-zero token cost, keeping prompt size virtually flat over hundreds of turns.
- **Toggle:** `export GW_DEEP_COMPACT=on` (defaults to `off`)
- **Parameters:**
  - `GW_DEEP_COMPACT_KEEP_TURNS` (default: `12`): Number of recent turns to preserve in full uncompacted detail.
  - `GW_DEEP_COMPACT_MAX_BYTES` (default: `500`): Byte threshold above which cold turn parts are compacted.
  - `GW_DEEP_COMPACT_HEAD_BYTES` (default: `200`): Preserved prefix bytes.
  - `GW_DEEP_COMPACT_TAIL_BYTES` (default: `100`): Preserved suffix bytes.

### Dynamic Sliding-Window Active Tool Pruning (`active_tool_pruning`)
Cline and other agent clients constantly advertise large lists of available tools (schemas) inside the request options on *every single turn*.
- **Active Tool Pruning (Strategy C):** Scans the history to see which tools have been executed.
- If an auxiliary, non-essential tool (e.g. `web_search`) has been called in the past but has gone "cold" (not called in the last `GW_ACTIVE_TOOL_PRUNING_WINDOW` turns), the gateway dynamically strips it from the allowed toolset (`opts.Tools`) on the active turn.
- **Self-Tuning Safety:** If a tool has *never* been called, it is kept active so the agent can invoke it for the first time whenever it wants. Essential core tools (configured via whitelist) are never pruned. This safely shaves off thousands of prompt tokens on long sessions.
- **Telemetry & Logging:** When tools are pruned, an `INFO` log detailing evaluated and selected tools (e.g., `Evaluated Tools: [a,b,c], Selected for Pruning: [b]`) is emitted for easy auditing. A `DEBUG` log tracks the exact bytes saved (calculated via JSON-marshaled size estimation), and propagates these savings to Prometheus under the metric key `cline_vertex_gw_compression_bytes_saved_total{stage="active_tool_pruning"}`.
- **Toggle:** `export GW_ACTIVE_TOOL_PRUNING=on` (defaults to `off`)
- **Parameters:**
  - `GW_ACTIVE_TOOL_PRUNING_WINDOW` (default: `20`): Active turns window for checking recent tool calls.
  - `GW_ACTIVE_TOOL_PRUNING_WHITELIST` (default: `"write_to_file,replace_in_file,execute_command,read_file,ask_followup_question,attempt_completion,new_task"`): Comma-separated list of whitelisted tool names that are immune to pruning (defaults to the absolute critical core system tools of Cline to maximize pruning utility while keeping human interaction and code-writing fully secure).

### Sliding Context Trimming (`trim`)
Enforces strict sliding-window caps based on character counts to prevent requests from exceeding your model's maximum context bounds. Older turns are gracefully removed from the conversation tail as newer turns are introduced.

---

## 2. Optimization Presets & Profiles

The gateway offers **5 progressive profiles** ranging from raw, unoptimized pass-through to maximum context squeeze. The **Default Profile (Profile 3)** sits right in the middle of the spectrum and requires no environment variables to be set.

### Summary Matrix

| Preset / Profile | Loop Traps & Nudge | Env Block Collapse | Tool Truncation | Deep Compaction | Active Tool Pruning | Replay Dedup | Stale Tool Pruning | Substring Dedup | Context Trim |
|---|---|---|---|---|---|---|---|---|---|
| **1. Pass-Through (Raw)** | Disabled | Disabled | Disabled | Disabled | Disabled | Disabled | Disabled | Disabled | Disabled |
| **2. Gentle/Conservative** | Active (No Nudge) | $\ge$ 1024 B | Disabled | Disabled | Disabled | $\ge$ 1024 B | Disabled | Disabled | Disabled |
| **3. Balanced (DEFAULT)** | Active + Nudge | $\ge$ 256 B | Active (8k limit) | Disabled | Disabled | $\ge$ 512 B | Disabled | Disabled | Disabled |
| **4. Aggressive** | Active + Nudge | $\ge$ 128 B | Active (4k limit) | Active ($\ge$ 12 turns) | Active (`window=20`) | $\ge$ 256 B | Enabled | Active ($\ge$ 512 B) | Soft limit |
| **5. Extreme Squeeze** | Active + Nudge | $\ge$ 64 B | Active (2k limit) | Active ($\ge$ 8 turns) | Active (`window=10`) | $\ge$ 128 B | Enabled | Active ($\ge$ 256 B) | Strict limit + Hard Output Caps |

---

### Detailed Profile Descriptions

#### 1. Pass-Through (Raw History)
- **Ideal for:** Debugging or zero-modification use cases. Disables all compressors; full, raw history is passed directly to Vertex.
- **Activation:**
  ```bash
  export GW_NORMALIZE_WHITESPACE=off
  export GW_COLLAPSE_ENV_BLOCKS=off
  export GW_DEDUP_REPLAY=off
  export GW_PRUNE_STALE_TOOLS=off
  export GW_MAX_INPUT_CHARS=0
  ```

#### 2. Gentle / Conservative
- **Ideal for:** Heavy code refactoring where you want absolutely zero risk of losing older environment details or minor tool outputs.
- **Activation:**
  ```bash
  export GW_COLLAPSE_ENV_THRESHOLD=1024
  export GW_DEDUP_THRESHOLD=1024
  export GW_TOOL_TRUNCATE_LIMIT=0
  export GW_PRUNE_STALE_TOOLS=off
  ```

#### 3. Balanced (Default Config)
- **Ideal for:** General software engineering tasks. Balanced defaults are active out-of-the-box with **zero environment variables** required.
- **Key Settings:** Whitespace clean-up is active; environment blocks collapse at 256 B; images/assets deduplicate at 512 B; tool results truncate at 8 KB.

#### 4. Aggressive Squeeze
- **Ideal for:** Rapid prototyping or exploratory chat where context costs dominate your workflow. Enables deep compaction for turns older than 12 turns and active tool pruning.
- **Activation:**
  ```bash
  export GW_COLLAPSE_ENV_THRESHOLD=128
  export GW_DEDUP_THRESHOLD=256
  export GW_DEDUP_SUBSTRING=on
  export GW_DEDUP_SUBSTRING_THRESHOLD=512
  export GW_TOOL_TRUNCATE_LIMIT=4096
  export GW_PRUNE_STALE_TOOLS=on
  export GW_DEEP_COMPACT=on
  export GW_DEEP_COMPACT_KEEP_TURNS=12
  export GW_ACTIVE_TOOL_PRUNING=on
  export GW_ACTIVE_TOOL_PRUNING_WINDOW=20
  ```

#### 5. Extreme Squeeze (Maximum Cost Reduction)
- **Ideal for:** Massively long agent loops running on lightweight models where you need to squeeze every token. Enables highly aggressive deep compaction for turns older than 8 turns and strict tool pruning.
- **Activation:**
  ```bash
  export GW_COLLAPSE_ENV_THRESHOLD=64
  export GW_DEDUP_THRESHOLD=128
  export GW_DEDUP_SUBSTRING=on
  export GW_DEDUP_SUBSTRING_THRESHOLD=256
  export GW_TOOL_TRUNCATE_LIMIT=2048
  export GW_PRUNE_STALE_TOOLS=on
  export GW_DEEP_COMPACT=on
  export GW_DEEP_COMPACT_KEEP_TURNS=8
  export GW_DEEP_COMPACT_MAX_BYTES=250
  export GW_DEEP_COMPACT_HEAD_BYTES=100
  export GW_DEEP_COMPACT_TAIL_BYTES=50
  export GW_ACTIVE_TOOL_PRUNING=on
  export GW_ACTIVE_TOOL_PRUNING_WINDOW=10
  export GW_ACTIVE_TOOL_PRUNING_WHITELIST="write_to_file,replace_in_file,execute_command"
  export GW_MAX_INPUT_CHARS=350000          # strict context tail trimming
  export GW_MAX_OUTPUT_TOKENS_HARD=4096     # strict generation limits
  ```
