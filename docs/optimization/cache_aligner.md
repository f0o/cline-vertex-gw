# 1b. Cache Aligner

The **Cache Aligner** stage is positioned at step `1b` of the optimization pipeline. It stabilizes the system instructions prefix by isolating and relocating volatile, frequently changing runtime variables to the system prompt's suffix.

---

## 🔍 Why It Matters

High-end LLM gateways (such as Anthropic Claude or Google Gemini) support **Prompt Caching** (KV caching). When a client sends a large system prompt and tool schema on consecutive requests, Google and Anthropic cache that prefix. Future requests that match this prefix exactly benefit from a **90% token pricing discount** and extremely fast response times.

However, client extensions like Cline append volatile runtime metadata directly into the system instructions or the very top of their prompt sequences, such as:
*   `Current Date & Time: 2026-06-11 07:48:00 UTC`
*   `Current Working Directory: /workspaces/project-alpha`
*   `Session ID: s_4b9a1c8`

Because the time and session ID change on **every single request**, the prefix is never identical. This single changing line at the top completely **busts/invalidates the KV cache**, forcing you to pay full input prices for the massive static instructions and tool schemas on every single turn.

**Cache Aligner** solves this by scanning the system instructions, extracting volatile context lines, and appending them to the very end under a structured separator. This keeps the massive static header prefix 100% stable, guaranteeing optimal prompt-caching hit rates.

---

## ⚙️ How It Works

This stage is implemented in `pkg/pipeline/cache_aligner.go` and executes as follows:

### 1. Volatile Prefix Identification
The engine splits the system prompt string by newline and inspects each line. It checks for common volatile, transient prefixes (case-insensitively):
*   `current date & time:` / `current date:` / `current time:` / `date & time:`
*   `current working directory:` / `working directory:`
*   `session id:` / `request id:`

### 2. Suffix Relocation
*   Lines matching these prefixes are extracted and placed into a `volatileLines` slice.
*   All other unchanging lines are placed into a `staticLines` slice.

### 3. Re-assembly
If any volatile lines are found, the engine reconstructs the system instructions:
1.  Appends all `staticLines` first.
2.  Appends a stable, clean separating boundary:
    ```text
    === Volatile Runtime Context (Stabilized at Suffix for Prefix Caching) ===
    ```
3.  Appends all extracted `volatileLines` at the very end.

This simple, clever relocation keeps the first 99% of the system instructions perfectly identical across turns, allowing Google and Anthropic to serve the KV cache with **100% efficiency**.

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_CACHE_ALIGNER` — Master toggle. Set to `false` or `0` to disable cache alignment. Defaults to `true` on Balanced.

---

## 📝 Example

### Before Optimization
```text
Current Date & Time: 2026-06-11 07:48:00 UTC
Current Working Directory: /workspaces/cline-vertex-gw
You are Cline, a highly skilled autonomous developer agent.
You have access to the following tools: ...
```

### After Optimization
```text
You are Cline, a highly skilled autonomous developer agent.
You have access to the following tools: ...

=== Volatile Runtime Context (Stabilized at Suffix for Prefix Caching) ===
Current Date & Time: 2026-06-11 07:48:00 UTC
Current Working Directory: /workspaces/cline-vertex-gw
```
*   **Result**: The changing date/time and directory lines are pushed to the end. The static instructions (representing 98% of the prompt) remain at the front, resulting in a perfect prompt cache hit on the next turn.
