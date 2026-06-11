# Prompt Optimization Pipeline

The gateway features a world-class, **14-stage in-flight prompt compression and context-optimization pipeline**. It is designed specifically to mitigate the extreme token inflation inherent in developer agent loops (such as Cline or Roo Code).

---

## 🛑 The "Complexity Trap" of Agentic Sessions

Autonomous developer agents work in iterative loops:
1.  **Read state**: The agent runs a CLI command or reads files to analyze a problem.
2.  **Act & Edit**: The agent modifies files or writes new code.
3.  **Validate**: The agent re-runs tests or reads the files again to confirm the edit.

Because LLMs are stateless, client-side extensions are forced to send the **entire conversation history** back to the model on every single turn. This history frequently contains massive repeated file payloads, stale command outputs, and identical IDE snapshots. 

In a 50-turn session, this "chatty" behavior causes prompt sizes to swell to hundreds of kilobytes, **increasing input costs quadratically** and diluting the model's attention span.

The **Prompt Optimization Pipeline** solves this problem. It intercepts outbound message arrays in real-time, executing lightweight semantic-aware and lossless pruning algorithms to **shave off up to 70-90% of prompt sizes** and maintain near-flat token costs on warm sessions.

---

## 🗺️ Pipeline Order of Operations

The execution order of pipeline stages is **load-bearing and structurally critical**. Each compressor is positioned in a specific sequence to maximize size savings without corrupting message pairing, system instructions, or tool schemas:

```
        Outbound genai.Content[] (Chat History) & System Prompt
                             │
                             ├─► [0. BreakLoopTrap]          (Deduplicates scoldings)
                             ├─► [0a. PruneActiveTools]      (Prunes cold active tools)
                             ├─► [0b. PruneStaleTools]       (Placeholder stale read-only tools)
                             │
                             ▼ (1. Normalization & Cache Alignment)
                             ├─► [1a. NormalizeWhitespace]   (Lossless line/space stripping)
                             ├─► [1b. AlignSystemPromptCache] (Relocates volatile metadata)
                             │
                             ▼ (2. Structural Elision & Truncation)
                             ├─► [2a. CollapseEnvBlocks]     (Stale <environment_details>)
                             ├─► [2b. TruncateToolResults]   (Dual-window progressive masking)
                             ├─► [2b1. ElideHistoricalWrites] (Elides write_to_file/diffs)
                             ├─► [2c. DeepCompactTurns]      (Compact cold historical turns)
                             │
                             ▼ (3. Sliding Budget Trimming)
                             ├─► [3. TrimContents]           (Slide turns to fit soft character cap)
                             │
                             ▼ (4. Backward Replay Deduplication)
                             ├─► [4. DedupReplayedBlocks]    (Whole-part text/image SHA-256 ptrs)
                             ├─► [4b. DedupSubstringBlocks]  (Contiguous substring ptrs)
                             │
                             ▼ (5. Upstream Compliance Formatting)
                             ├─► [5. AlignFunctionCalls]     (Enforces strict tool alternation)
                             │
                 Optimized Outbound Payload for Upstream
```

### Why This Order Matters:
1.  **Normalize & Collapse First**: Standard formatting occurs before budget checks so that `TrimContents` calculates character lengths against already-shrunk payloads, preventing premature turn drops.
2.  **Prune Early, Dedup Late**: Loopbreaks and stale tools are pruned early so later stages don't waste CPU or cache space on dead turns. Deduplication occurs last so that pointers are resolved only on the active subset of messages actually being sent.
3.  **Caching Tagging Occurs Post-Compression**: Adapters tag ephemeral cache-control headers (`cache_control`) *after* the pipeline has finalized the history layout, ensuring prompt cache blocks are perfectly aligned.

---

## 🎛️ The 5 Profile Presets

Operators can set the master toggle variable `GW_PROFILE` to instantly load one of the progressive presets, or override specific toggles via individual `GW_*` env parameters:

*   **Profile 1: Pass-Through (`passthrough`)** — All optimizations are completely disabled. Delivers the client history unchanged. Useful for raw model evaluation and A/B benchmarking.
*   **Profile 2: Gentle (`gentle`)** — Zero-risk, lossless formatting optimizations. Minimizes horizontal whitespaces, removes carriage returns, aligns cache blocks, and collapses stale `<environment_details>` snapshots.
*   **Profile 3: Balanced (`balanced`)** — **The default configuration**. Maximizes cost efficiency with minimal regression risk. Activates progressive tool-result truncation, file write action elision, and multi-turn whole-part replay deduplication.
*   **Profile 4: Aggressive (`aggressive`)** — Turns on more invasive, experimental compressors. Activates stale read-only tool pruning, sub-string partial deduplication, and active toolset filtering.
*   **Profile 5: Extreme Squeeze (`squeeze`)** — Maximum compression. In addition to Profile 4, it deep-compacts cold turns older than 8 messages, reduces tool outputs to extremely tight boundaries, and enforces a hard soft-limit cap on overall context.

---

## 🔄 The Lossless Compress-Cache-Retrieve (CCR) Loop

Several of the gateway's advanced compression stages (such as **Truncate Tool Results**, **Write Action Elision**, and **Deep Compaction**) are physically *lossy* inside the active message history, replacing thousands of code or stdout characters with short lookup hashes.

To prevent model confusion or data corruption, **the gateway makes these elisions completely lossless and reversible**:

1.  **Compress**: The compiler hashes the elided content (e.g. `write_to_file` body) using SHA-256 and writes it as an atomic file to a local cache directory (`~/.cache/cline-vertex-gw/elided_<hash>.json`).
2.  **Cache**: The content in the chat turn is replaced with a standard tombstone marker, such as:
    `[Content elided: 12,450 bytes. Retrieve full content: hash=3a8f12c...]`
3.  **Expose**: The gateway detects the presence of these tombstone markers in the history and dynamically injects the helper tool `retrieve_elided_content` into the model's allowed active toolset.
4.  **Retrieve**: If the model decides it needs to see the exact code or output of an older turn, it calls:
    `retrieve_elided_content(hash="3a8f12c...")`
5.  **Local Intercept**: The gateway intercept this call, reads the JSON file from the local FSCache, and injects the raw unelided text directly back into the conversation history, restarting the stream. The client never sees this loop and no network round-trip is wasted.

### ⚠️ The Recursive Elision Loop-Bypass Invariant
A critical security and correctness safeguard built into the engine (`pkg/pipeline/ccr_helpers.go`):
*   **The Trap**: When the model calls `retrieve_elided_content`, the gateway retrieves and injects the raw text into the history. When this updated history is sent upstream, the gateway runs the prompt compression pipeline again. Without a bypass, the pipeline would see this older raw text, recognize it as older than the retain window, and **immediately elide/truncate it back to a placeholder**. This traps the model in an infinite recursive retrieval loop.
*   **The Bypass**: The gateway integrates `HasRetrievalToolCall(contents)`. If any `retrieve_elided_content` tool call or response is active in the message history, the gateway **completely bypasses all lossy compression stages** (`TruncateToolResults`, `ElideHistoricalWriteActions`, `DeepCompactHistoricalTurns`, and `PruneStaleTools`). This ensures retrieved raw text reaches the model in full, preserving flawless editing and tool execution.
