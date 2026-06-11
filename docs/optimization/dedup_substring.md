# 4b. Substring Block Dedup

The **Substring Block Dedup** stage is positioned at step `4b` of the optimization pipeline. It performs multi-turn partial contiguous substring deduplication, collapsing embedded copies of earlier large text parts inside later turns.

---

## 🔍 Why It Matters

Whole-part deduplication (`DedupReplayedBlocks`) is incredibly effective, but only fires on **100% exact matches**. In real-world developer agent loops, the duplication patterns can be subtler:
*   **The Edited File**: The model reads a 2,000-line source file, makes a small 2-line edit using a tool, and then re-reads the entire file. Because the file has been edited, the new 2,000-line block is **not** byte-identical to the earlier one, causing whole-part dedup to miss it.
*   **Embedded Repastes**: A tool result carries a large console stream. In a subsequent turn, that exact same stream is printed again but wrapped inside a slightly larger test-runner output block.

In both cases, a huge chunk of text is repeated verbatim, but since it is embedded inside a slightly different wrapper, whole-part dedup cannot collapse it.

**Substring Block Dedup** solves this by treating earlier text parts as "needles" and scanning later same-role text parts. If a needle matches contiguously inside a later part, that segment is collapsed to a back-reference placeholder, leaving the surrounding new text fully intact.

---

## ⚙️ How It Works

This stage is implemented in `pkg/pipeline/dedup_substring.go` and executes as follows:

### 1. Needle Collection
The engine walks through the conversation history and collects all text parts that clear the `GW_DEDUP_SUBSTRING_MIN_BYTES` threshold (default: `1024` bytes) to serve as search "needles". 
*   Needles are tracked along with their originating turn index and the speaker's role.

### 2. Longest Needle Sort & Matching
To guarantee that the most valuable collapse occurs, needles are sorted by length in descending order (longest first). For each text part in a subsequent turn:
*   **Role Constraint**: It only searches for needles originating from the same speaker's role (user-to-user, assistant-to-assistant), preventing role cross-talk.
*   **Strict Contiguity**: The needle must exist as an exact, contiguous verbatim substring. No fuzzy or partial token-overlap matches are performed, ensuring **zero risk of deleting unique text**.

### 3. Substitution & Needle Generation
If a match is found, the engine substitutes the contiguous segment with a back-pointer:
```text
[12450 bytes elided: identical block already shown in turn 5]
```
The surrounding text remains untouched. The newly collapsed block is itself recorded as a future needle (if it still clears the size threshold) to allow multi-layered nesting collapses on later turns.

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_DEDUP_SUBSTRING` — Master switch. Set to `true` or `1` to enable. Defaults to `false` (disabled) on Balanced, and `true` on Aggressive and Extreme Squeeze.
*   `GW_DEDUP_SUBSTRING_MIN_BYTES` (alias `GW_DEDUP_SUBSTRING_THRESHOLD`) — Minimum size threshold for substring deduplication. Defaults to `1024` bytes.

---

## 📝 Example

### Before Optimization
```text
Turn 5 [User]: (File content)
  package main
  import "fmt"
  func main() { fmt.Println("Hello") }

Turn 6 [Model]: Replace "Hello" with "World".
Turn 7 [User]: Verified file contents:
  package main
  import "fmt"
  func main() { fmt.Println("World") }
```

### After Optimization
```text
Turn 5 [User]: (File content)
  package main
  import "fmt"
  func main() { fmt.Println("Hello") }

Turn 6 [Model]: Replace "Hello" with "World".
Turn 7 [User]: Verified file contents:
  [64 bytes elided: identical block already shown in turn 5]
  func main() { fmt.Println("World") }
```
*   **Result**: The repeated boilerplate is collapsed, while the actual modification remains visible.
