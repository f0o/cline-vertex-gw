# 2b1. Write Action Elision

The **Write Action Elision** stage is positioned at step `2b1` of the optimization pipeline. It scans historical model turns for massive file write or file modification tool calls (specifically `write_to_file` and `replace_in_file`), eliding their massive payloads to save thousands of tokens.

---

## 🔍 Why It Matters

Developer agents frequently write new files or apply extensive changes to existing workspaces:
*   A model emits a `write_to_file` call carrying **25,000 bytes** of code.
*   In subsequent turns, the model and client continue interacting, and the write turn slides deeper into the history.

The actual code body or diff payload inside the `write_to_file` or `replace_in_file` call represents dead weight in the model's history. The model does not need to re-read the exact code block it wrote 5 turns ago—it has since read the file back via `read_file` or executed compiler checks. Carrying this massive payload turn-after-turn quickly swells token counts and balloons expenses.

**Write Action Elision** resolves this by intercepting historical assistant turns and replacing their file-writing arguments (`content` and `diff`) with clean, informative placeholder text, while **losslessly preserving the raw code body in FSCache**.

---

## ⚙️ How It Works

This stage is implemented in `pkg/pipeline/write_elision.go` and executes as follows:

### 1. Identify History Boundaries
The engine walks backwards to find the index of the latest turn. It calculates the distance of each message from this turn:
*   **Safety Zone Exemption**: Any model turn falling within the `GW_WRITE_ACTION_RETAIN_WINDOW` (default: `3` turns) is left completely unelided. This ensures that the model can see the exact code blocks it wrote in its active/recent history, preventing it from repeating or referencing stale placeholders.

### 2. Locate Write Tool Calls
For all non-exempt older turns, the engine parses model parts looking for specific `FunctionCall` blocks:
*   `write_to_file` (targeting the `"content"` argument key)
*   `replace_in_file` (targeting the `"diff"` argument key)

### 3. Compress-Cache-Retrieve (CCR) Elision
If a match is found:
*   **Threshold check**: The engine checks if the target argument is a string and its length exceeds **2048 bytes**. Tiny writes are skipped because their compression yields negligible savings.
*   **Write to Cache**: The original raw string is hashed with SHA-256 and written to the local cache directory (`~/.cache/cline-vertex-gw/elided_<hash>.json`).
*   **Copy-on-Write Substitution**: The engine copies the `Part` and `FunctionCall` structures to avoid concurrent map mutations, replacing the large argument with a short placeholder carrying the lookup hash:
    `"[Content: 25420 bytes written. Elided. Retrieve full content: hash=3a8f12c...]"` (or `[Diff: ...]`)

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_WRITE_ACTION_ELISION` — Master switch. Set to `false` or `0` to disable write action elision. Defaults to `true` on Balanced.
*   `GW_WRITE_ACTION_RETAIN_WINDOW` — Sliding turn window size to keep write parameters unelided. Defaults to `3` turns on Balanced.

---

## 📝 Example

### Before Optimization
```text
Turn 4 [Model]: Call write_to_file(path="app.go", content="package main\n\nimport ... [25,000 bytes of code]")
Turn 5 [User]: Response from write_to_file: {"status":"success"}
...
Turn 12 [Model]: (Current active turn)
```

### After Optimization
```text
Turn 4 [Model]: Call write_to_file(path="app.go", content="[Content: 25000 bytes written. Elided. Retrieve full content: hash=3a8f12c...]")
Turn 5 [User]: Response from write_to_file: {"status":"success"}
...
Turn 12 [Model]: (Current active turn)
```
*   **Result**: The large code payload in Turn 4 is compressed to a short placeholder, saving 25,000 tokens while remaining fully recoverable if the model invokes `retrieve_elided_content` later.
