# 3. Context Budget Trim

The **Context Budget Trim** stage is positioned at step `3` of the optimization pipeline. It enforces a sliding-window character budget across the conversation history, dropping the oldest turns first to guarantee that the request fits within upstream limits or operator limits.

---

## 🔍 Why It Matters

Foundation models have strict physical boundaries on their maximum allowed context windows. While models like Gemini support massive 1M+ contexts, keeping huge logs active is incredibly expensive. Additionally, other models in the model garden have tighter ceilings (e.g. 8K or 32K). 

When a long-running session expands, sending too many messages will either:
1.  Saturate context budgets, triggering a failure or abrupt disconnection.
2.  Waste massive amounts of tokens on obsolete historical details.

The **Context Budget Trim** stage provides an insurance policy. It guarantees that the cumulative length of your prompt system instructions and conversation turns never exceeds a soft character budget.

---

## ⚙️ How It Works

This stage is implemented in `pkg/pipeline/budget.go` and executes in four steps:

### 1. Fast-Path Check
If `GW_MAX_INPUT_CHARS` is set to `0` (default on Balanced), the trimmer is bypassed entirely. This prevents any computational or allocation overhead on normal sessions.

### 2. System Instructions Safeguard
The trimmer calculates the length of the system prompt in characters. 
*   **Safety Rule**: The system instructions are **never trimmed**. Since system instructions hold the model's behavioral programming, rules of engagement, and available tool lists, dropping them would break the agent entirely.
*   **Degenerate Fallback**: If the system instructions *alone* exceed the budget, the trimmer keeps only the latest (most recent) turn and drops all other turns, as there is nothing else it can safely trim.

### 3. Cumulative Length Evaluation
The trimmer sums up the approximate byte/character lengths of all non-nil conversation turns. If the cumulative size is within `GW_MAX_INPUT_CHARS`, the entire history is passed through intact.

### 4. Sliding-Window Dropping
If the budget is exceeded, the engine walks from the newest turn backwards, accumulating size weights. 
*   It adds turns to a "kept" list until the next turn would exceed the remaining character budget.
*   **The Latest Turn Shield**: The latest user turn (the question currently being asked) is **always preserved** under the `minRetainedTurns = 1` safeguard.
*   Older turns that do not fit into the accumulated weight list are dropped entirely, sliding the window forward.

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_MAX_INPUT_CHARS` — Soft context budget character ceiling. Set to `0` to disable trimming. Defaults to `0` (disabled) on Balanced, and `350000` chars on Extreme Squeeze.

---

## 📝 Example

### Context Setup
*   **Budget**: `100,000` characters.
*   **System Instructions**: `20,000` characters (Remaining budget: `80,000` characters).
*   **History**: 10 turns.
    *   Turn 1 to 4: Older turns containing massive raw terminal outputs (totaling `120,000` characters).
    *   Turn 5 to 10: Recent interactions and the latest question (totaling `45,000` characters).

### Before Optimization
The prompt totals `185,000` characters, exceeding our budget of `100,000`.

### After Optimization
The engine walks backwards, keeping Turn 10 down to Turn 5 (totaling `45,000` characters). Adding Turn 4 would push the total to `165,000`, which exceeds the budget. 
The engine drops Turn 1 to 4 entirely, passing through only Turn 5 to 10, **saving 120,000 characters (approx. 35,000 tokens)**.
