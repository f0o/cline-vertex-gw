# 2. Collapse Env Blocks

The **Collapse Env Blocks** stage is positioned at step `2` of the optimization pipeline. It targets and collapses stale, historical `<environment_details>` blocks emitted by Cline on older turns, saving thousands of tokens per request.

---

## 🔍 Why It Matters

Cline appends an extensive XML block formatted as `<environment_details>...</environment_details>` to the end of **every single user turn**. 

This block holds a comprehensive dump of the active workspace state:
*   Open tabs in the editor.
*   The current working directory.
*   A recursive list of files and folders in the workspace.
*   Active terminal outputs and backgrounds.
*   The IDE's current execution mode.

Across a 30-turn session, this massive snapshot gets re-shipped verbatim on every single turn. However, **only the final (most recent) turn's snapshot matters** for model reasoning about the current environment. All older snapshots are completely stale and dilute the active context with hundreds of thousands of redundant characters.

**Collapse Env Blocks** solves this by scanning all older user turns and replacing their `<environment_details>` bodies with a tiny, one-line placeholder, while **explicitly preserving the final turn's snapshot verbatim** so the model remains fully aware of the current IDE state.

---

## ⚙️ How It Works

This stage is implemented in `pkg/pipeline/envblocks.go` and executes as follows:

### 1. Identify the Latest User Turn
The engine walks backward from the end of the history to locate the index of the **last non-nil user turn**. This turn is marked as **exempt** and will never be touched, keeping the freshest environment details fully intact for the model.

### 2. Simple Non-Nested Tag Scanning
The engine walks through all non-exempt user turns and parses their text parts. Because Cline does not nest environment blocks, the engine uses a simple, highly efficient, and safe substring scanner. It locates the literal boundaries:
*   `openTag`: `<environment_details>`
*   `closeTag`: `</environment_details>`

### 3. Threshold & Placeholder Replacement
If a valid block is found, the engine calculates its length in bytes:
*   **Size Gate**: If the block is smaller than `GW_COLLAPSE_ENV_MIN_BYTES` (default: `256` bytes), it is left alone, as collapsing tiny blocks would cost more placeholder bytes than it saves.
*   **Replacement**: If it clears the gate, the body is removed and replaced with a tight, informative placeholder, while retaining the tags to preserve XML-structure expectations:
    ```text
    <environment_details>[12,450 bytes elided: stale IDE snapshot from earlier turn]</environment_details>
    ```

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_COLLAPSE_ENV_BLOCKS` — Master switch. Set to `false` or `0` to disable env-block collapsing. Defaults to `true` on Balanced.
*   `GW_COLLAPSE_ENV_MIN_BYTES` (alias `GW_COLLAPSE_ENV_THRESHOLD`) — Size threshold for env-block collapsing. Defaults to `256` bytes on Balanced.

---

## 📝 Example

### Before Optimization
```text
Turn 5 [User]: I ran the tests and they failed.
<environment_details>
Working Directory: /workspaces/cline-vertex-gw
Open Tabs: main.go, handlers.go
Files:
  - main.go
  - pkg/api/handlers.go
  - pkg/provider/vertex.go
Terminal Output:
  - make test ➔ Exit Code 1: TestAlignFail
</environment_details>
```

### After Optimization
```text
Turn 5 [User]: I ran the tests and they failed.
<environment_details>[284 bytes elided: stale IDE snapshot from earlier turn]</environment_details>
```
*   **Result**: The older IDE state dump is collapsed to a short, informative placeholder, reclaiming substantial context space while keeping the system prompt expectations completely aligned.
