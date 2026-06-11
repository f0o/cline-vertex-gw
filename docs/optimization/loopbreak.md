# 0. Loop Break Trap

The **Loop Break Trap** stage is positioned at step `0` of the optimization pipeline. It automatically detects and resolves stuck-in-a-loop deadlocks caused by repetitive client-side automated scoldings and empty assistant responses.

---

## 🔍 Why It Matters

Autonomous developer agents (like Cline) are governed by strict instructions on how they must format tool arguments. When a model fails to call a tool properly (or returns an empty response without making any tool calls), the client extension frequently inserts an automated warning or scolding user turn back into the chat history, such as:
*   `"You did not use a tool in your previous response! Please retry with a tool use."`
*   `"TODO LIST UPDATE REQUIRED: You MUST include the task_progress parameter."`

When this scolding is re-appended to the prompt, the model can get confused and output *another* empty text response. The client then re-appends *another* scolding turn. This creates an inescapable infinite loop where the model wastes thousands of tokens producing nothing but empty replies.

The **Loop Break Trap** resolves this by intercepting and pruning old duplicate scoldings and empty assistant replies, and then appending a direct "action nudge" to the final turn to force the model back into active task execution.

---

## ⚙️ How It Works

The loop-trap breaker is implemented in `pkg/pipeline/loopbreak.go` and executes in three distinct steps:

### 1. Scolding Classification
The engine scans the entire conversation history from first to last turn, checking each `user` message's text parts for specific scolding sentences using the `isScoldingTurn` parser:
*   `"You did not use a tool in your previous response!"`
*   `"TODO LIST UPDATE REQUIRED"` / `"You MUST include the task_progress parameter"`

### 2. Duplicate Pruning with Safety Guards
If multiple scoldings are found in the history, older ones are marked for deletion alongside the *subsequent* empty model turn. This preserves perfect **User-Model-User role alternation**.

To prevent state corruption, the engine implements several **unbreakable safety guards**:
*   **Initial Turn Exemption**: It never drops turn `i == 0` (first user turn) as that contains the core user request.
*   **Model Tool Call Shield**: A model turn is only dropped if it has **exactly zero function calls**. If the model made a function call, the preceding scolding and model turn are left intact to keep the execution history aligned.
*   **User Response Shield**: A user turn is never classified as a scolding if it carries any `FunctionResponse` (tool result).

### 3. Action Nudging
If the final/latest turn in the history is a scolding turn and nudge is enabled (`GW_LOOP_TRAP_NUDGE=true`), the engine appends a concrete, clear tool-use nudge to the text:
```text
If you have no other tools to call, please execute 'pytest' in the terminal using the execute_command tool to verify the environment state and continue.
```
This gives the model a clear, valid tool objective, instantly breaking the empty-output loop.

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_BREAK_LOOP_TRAP` — Master switch. Set to `false` or `0` to disable loop resolution. Defaults to `true` on Balanced.
*   `GW_LOOP_TRAP_NUDGE` — Enables appending the helpful, clear tool-use nudge. Defaults to `true` on Balanced.

---

## 📝 Example

### Before Optimization
```text
Turn 12 [User]: You did not use a tool in your previous response! Please retry with a tool use.
Turn 13 [Model]: (empty response, 0 tokens)
Turn 14 [User]: You did not use a tool in your previous response! Please retry with a tool use.
Turn 15 [Model]: (empty response, 0 tokens)
Turn 16 [User]: You did not use a tool in your previous response! Please retry with a tool use.
```

### After Optimization
```text
Turn 16 [User]: You did not use a tool in your previous response! Please retry with a tool use.

If you have no other tools to call, please execute 'pytest' in the terminal using the execute_command tool to verify the environment state and continue.
```
*   **Result**: The gateway drops turns 12, 13, 14, and 15 entirely, saving thousands of tokens, and appends the action nudge to Turn 16, successfully breaking the deadlock.
