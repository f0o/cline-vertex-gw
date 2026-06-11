# 4. Duplicate Block Dedup

The **Duplicate Block Dedup** stage is positioned at step `4` of the optimization pipeline. It executes multi-turn whole-part replay deduplication across the conversation history, collapsing identical re-pastes of large text blocks and screenshots into clean, backwards-pointing pointers.

---

## 🔍 Why It Matters

Autonomous developer agents (like Cline) frequently replay identical content across consecutive turns:
*   **The Re-read Habit**: The model reads a source code file, edits it, and then re-reads the *entire* file again to confirm. 
*   **Screenshot Loops**: When analyzing visual states, Cline's default workflow takes a **desktop screenshot on every single turn**. This means an identical, high-resolution 1 MB image gets sent over and over again on Turn 10, Turn 11, Turn 12, etc.

These redundant blocks represent massive token waste. Sending the same 30 KB source file 10 times costs 300 KB, even though the file only changed in one location.

**Duplicate Block Dedup** resolves this. It walks through the active conversation window and replaces verbatim repeats of large text or image parts with a one-line back-reference pointer.

---

## ⚙️ How It Works

This stage is implemented in `pkg/pipeline/dedup.go` and executes as follows:

### 1. Zero-Allocation String Hashing
Instead of copying strings into byte slices (which copies megabytes of memory onto the heap, causing garbage collection spikes), the engine writes strings directly using Go's `io.WriteString` on a `sha256` hasher, executing at **exactly 0 memory allocation overhead**.

### 2. Role-Scoped Hash Tracking
The engine walks through the history and tracks parts:
*   **The Key**: Hash keys are scoped by the speaker's role (`firstSeen[role+kind+hash]`), such as:
    `user|text|9b4a1c8f3e2d1a0b`
*   **Safety**: Speaker scoping is semantically crucial. A user turn's content is only pointed at an *earlier user turn*, never an assistant turn, preventing role confusion.
*   **Backward Pointing**: Pointers always point **backward** in time. The very first occurrence of a block is always kept verbatim. Only subsequent repeats are collapsed.

### 3. Image & Text Collapse
If a duplicate is found, the engine replaces it with a clean text pointer:
*   **Text Parts**: Overwritten with:
    `"[25400 bytes elided: identical content already shown in turn 5 (sha256=9b4a1c8f...)]"`
    *   **Size Gate**: Only text parts larger than `GW_DEDUP_MIN_BYTES` (default: `512` bytes) are eligible.
*   **Image Parts**: Image duplicates are collapsed unconditionally (even a 16x16 icon), replacing the heavy base64 block with:
    `"[image elided: identical image/png image (850400 bytes, sha256=3a8f12c8...) already shown in turn 4]"`

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_DEDUP_REPLAY` — Master switch. Set to `false` or `0` to disable replay deduplication. Defaults to `true` on Balanced.
*   `GW_DEDUP_MIN_BYTES` (alias `GW_DEDUP_THRESHOLD`) — Minimum size threshold for text-part deduplication. Defaults to `512` bytes on Balanced.

---

## 📝 Example

### Before Optimization
```text
Turn 4 [User]: Code file payload:
  package main
  import "fmt"
  func main() { fmt.Println("Hello") }

Turn 5 [Model]: Call a tool.
Turn 6 [User]: Response.
Turn 7 [User]: Code file payload:
  package main
  import "fmt"
  func main() { fmt.Println("Hello") }
```

### After Optimization
```text
Turn 4 [User]: Code file payload:
  package main
  import "fmt"
  func main() { fmt.Println("Hello") }

Turn 5 [Model]: Call a tool.
Turn 6 [User]: Response.
Turn 7 [User]: Code file payload:
  [64 bytes elided: identical content already shown in turn 4 (sha256=9b4a1c8f...)]
```
*   **Result**: Reclaims identical multi-turn bytes, keeping context sizes minimal.
