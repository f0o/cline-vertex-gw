# 2b. Tool Result Truncation

The **Tool Result Truncation** stage is positioned at step `2b` of the optimization pipeline. It executes **Dual-Window Progressive Observation Masking** to aggressively compress bulky older tool execution output (like `read_file` results or terminal console streams), saving massive amounts of context.

---

## 🔍 Why It Matters

Observation data (such as compiler output dumps, large file reads, or massive test failures) dominates over 80% of token consumption in active agentic development loops. 

A model needs to see the exact text output at the moment it executes a tool to understand the compiler or environment state. However, on older turns, the model rarely needs to re-read all 3,000 lines of console logs or source code files. It merely needs to remember *that* it executed the tool, and *roughly* what the results were.

JetBrains' research ("The Complexity Trap" at NeurIPS 2025) proves that simple progressive observation masking matches or exceeds expensive, slow LLM-summarization techniques at a fraction of the compute and token cost.

**Tool Result Truncation** leverages this insight. It divides the chat history into a **Near-History window** and a **Deep-History zone**, applying progressive elisions and preserving the newest interaction completely intact.

---

## ⚙️ How It Works

This stage is implemented in `pkg/pipeline/toolresult.go` and processes `FunctionResponse` parts as well as plain text parts representing flattened tool outputs:

### 1. Identify the Latest Turn
The latest non-nil message in the history is **completely exempt** and left unelided. This ensures the freshest tool output is preserved in full, as this is what the model is reasoning about right now.

### 2. Dual-Window Progressive Masking
For all older turns, the engine calculates their distance from the latest turn and applies progressive rules:

#### A. The Near-History Window (Middle-Elision)
If a turn falls within the `GW_TOOL_RESULT_RETAIN_WINDOW` (default: `3` turns), it is left unelided. If it falls slightly outside this window but is still close (e.g. less than 2 turns deeper), the engine applies **Middle-Elision** via `middleElide`:
*   **Head & Tail Preservation**: It keeps the first `GW_TOOL_RESULT_HEAD_BYTES` (default: `2000` bytes) and last `GW_TOOL_RESULT_TAIL_BYTES` (default: `1000` bytes) of the text.
*   **Why**: Keeping the head and tail preserves critical metadata—like file imports and structures at the top, and trailing compiler warnings, error stacks, or exit codes at the bottom—while dropping the bulky middle body.
*   **Newline Snapping**: Cut boundaries are snapped to the nearest line boundaries within a 256-byte window (`snapToNewline`) so code or text lines are never cut mid-sentence.
*   **Lossless Cache**: The original raw string is saved to FSCache using SHA-256 before cutting, returning a reversible lookup hash.

#### B. The Deep-History Zone (Complete Masking)
If a turn falls deep in the history (distance $\ge \text{Retain Window} + 2$), the engine applies **Complete Masking** via `completeElide`:
*   **The Tombstone**: The entire text is stripped and replaced with a compact, one-line placeholder:
    `[Tool output masked: 124,520 bytes elided. Retrieve full content: hash=3a8f12c...]`
*   **Lossless Cache**: The entire payload is written to FSCache and remains dynamically retrievable on-demand.

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_TOOL_RESULT_TRUNCATE` — Master switch. Set to `false` or `0` to disable. Defaults to `true` on Balanced.
*   `GW_TOOL_RESULT_MAX_BYTES` (alias `GW_TOOL_TRUNCATE_LIMIT`) — Only tool results larger than this are eligible for truncation. Defaults to `8000` bytes on Balanced.
*   `GW_TOOL_RESULT_HEAD_BYTES` — Head bytes kept in truncated tool outputs. Defaults to `2000` bytes on Balanced.
*   `GW_TOOL_RESULT_TAIL_BYTES` — Tail bytes kept in truncated tool outputs. Defaults to `1000` bytes on Balanced.
*   `GW_TOOL_RESULT_RETAIN_WINDOW` — Turns to keep completely unelided. Defaults to `3` turns on Balanced.

---

## 📝 Example

### Before Optimization (Near-History)
```text
Turn 10 [User]: Response from read_file (12,000 bytes of main.go)
  package main
  import "fmt"
  // ... 8,000 bytes of code ...
  func main() {
      fmt.Println("Done")
  }
```

### After Optimization (Middle-Elision)
```text
Turn 10 [User]: Response from read_file
  package main
  import "fmt"

  … 9000 bytes elided (tool result truncated for older turn). Retrieve full content: hash=3a8f12c…

  func main() {
      fmt.Println("Done")
  }
```
*   **Result**: The code imports and main execution function are preserved in place. The massive middle body is stored in local FSCache, reclaiming 9,000 tokens while remaining fully recoverable via the `retrieve_elided_content` tool.
