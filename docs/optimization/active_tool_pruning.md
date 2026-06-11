# 0a. Active Tool Pruning

The **Active Tool Pruning** stage is positioned at step `0a` of the optimization pipeline. It dynamically scales down the allowed active toolset by pruning cold, unused auxiliary tools on a per-request basis.

---

## 🔍 Why It Matters

High-capability developer agents (like Cline) declare massive tool-calling schemas containing up to 10–15 functions (such as `read_file`, `write_to_file`, `replace_in_file`, `search_files`, `list_files`, `execute_command`, `ask_followup_question`, `attempt_completion`, etc.). 

Each tool schema carries descriptive parameter structures, type declarations, and inline instructions. These schemas can easily consume **3,000 to 8,000 tokens** per request. 

However, during deep engineering sessions, the model frequently settles into a repetitive sub-pattern, calling only `write_to_file` and `execute_command` for many turns in a row. Carrying the other 10 unused tool schemas on every single turn is an expensive waste of tokens.

**Active Tool Pruning** solves this by scanning the conversation history. If an auxiliary tool has been used in the past, but has not been called within a configurable rolling window of turns, it is dynamically pruned from the request's allowed toolset.

---

## ⚙️ How It Works

This stage is implemented in `pkg/pipeline/active_tool_pruning.go` and executes as follows:

### 1. Whitelist IMMUNITY Check
The engine parses the comma-separated whitelist (`GW_ACTIVE_TOOL_PRUNING_WHITELIST`). Any tool matching a whitelisted name is immune and **is never pruned**. This guarantees core operations remain active at all times.
*   **Default Whitelist**: `"write_to_file,replace_in_file,execute_command,read_file,ask_followup_question,attempt_completion,new_task"`

### 2. History Activity Scan
The engine scans the conversation history to map tool usage:
*   It records all tools that have **ever** been called in the history (`usedTools`).
*   It records tools called within the most recent window of turns (`recentlyUsedTools`), defined by `GW_ACTIVE_TOOL_PRUNING_WINDOW` (default: `20` turns).

### 3. Pruning Rule Application
For each tool declared in the client request (`opts.Tools`), the engine evaluates whether to keep or drop it:
*   **Rule**: If a tool has been used in the past (`usedTools[name] == true`), but has **not** been used recently (`recentlyUsedTools[name] == false`), and is **not** on the whitelist, it is pruned.
*   **Exemption**: If a tool has **never** been called in the past, it is kept verbatim. This ensures the model can invoke new tools for the first time.

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_ACTIVE_TOOL_PRUNING` — Master toggle. Set to `true` or `1` to enable. Defaults to `false` (disabled) on Balanced, and `true` on Aggressive and Extreme Squeeze.
*   `GW_ACTIVE_TOOL_PRUNING_WINDOW` — Sliding turn window size for monitoring tool activity. Defaults to `20` turns on Balanced, and `10` turns on Extreme Squeeze.
*   `GW_ACTIVE_TOOL_PRUNING_WHITELIST` — Comma-separated list of immune tools. Defaults to core Cline tools.

---

## 📝 Example

### Context Setup
*   **Window**: 10 turns.
*   **Auxiliary Tool**: `list_code_definition_names` (Not whitelisted).
*   **Session State**: The model called `list_code_definition_names` on turn 3. We are now on turn 25. For the last 22 turns, the model has only called `execute_command`.

### Before Optimization
The gateway receives the chat history along with 10 tool definitions, including `list_code_definition_names`.

### After Optimization
The gateway detects that `list_code_definition_names` was called on turn 3, which is older than our 10-turn window, and has not been called since. It dynamically strips `list_code_definition_names` from the active tool list before sending the request to Vertex AI, **saving thousands of tokens**.
