# 0b. Stale Tool Pruning

The **Stale Tool Pruning** stage is positioned at step `0b` of the optimization pipeline. It dynamically matches and prunes superseded, historical read-only tool exchanges from the conversation history.

---

## 🔍 Why It Matters

Autonomous developer agents frequently re-examine the same workspace parameters repeatedly. For example:
1.  Model calls `read_file` on `pkg/pipeline/config.go` (Turn 5).
2.  Model edits `pkg/pipeline/config.go` using `replace_in_file` (Turn 7).
3.  Model calls `read_file` on `pkg/pipeline/config.go` again to verify the edit (Turn 9).

In this scenario, the older file dump from Turn 5 is completely stale—the file content has since been modified and re-read. Holding onto the massive Turn 5 `read_file` call and response blocks dilutes the active context, inflates costs, and adds zero utility for subsequent reasoning.

**Stale Tool Pruning** solves this by scanning the history for identical read-only tool calls. If an identical tool is invoked later with the same arguments, the earlier call and response are pruned and replaced with lightweight, short placeholder text.

---

## ⚙️ How It Works

This stage is implemented in `pkg/pipeline/prune_tools.go` and executes in three distinct steps:

### 1. Identify Paired Read-Only Exchanges
The engine scans the conversation history to locate paired read-only tool calls and their responses. 
*   **Idempotency Gate**: Only strictly **read-only/idempotent** tools are eligible. Mutating tools (like `write_to_file` or `execute_command`) are immune and are never touched.
    *   **Eligible Tools**: `read_file`, `list_files`, `search_files`, `list_code_definition_names`.
*   **Response Pairing Safety**: It matches model turns containing tool calls to user turns containing tool responses. Pairing is done **by ID first** (highly robust for OpenAI/MaaS streams) and falls back to **Name matching** second.

### 2. Identify and Mark Superseded Duplicates
For each eligible tool, the engine computes a stable argument key using a deterministic string rendering of the arguments (`argsStableKey`):
```text
read_file\x00path=pkg/pipeline/config.go;
```
If multiple exchanges share the same argument key, they are sorted chronologically. **The latest (newest) exchange is kept intact**, while all earlier duplicates are marked for pruning.

### 3. Part-Level Placeholder Replacement
Instead of dropping whole turns (which can disrupt role alternation or delete important context like model thoughts), the engine executes **part-level replacement**. 
*   Only the specific parts corresponding to the duplicated `FunctionCall` and `FunctionResponse` are rewritten.
*   **Placeholder Marks**:
    *   The `FunctionCall` part is replaced with: `"(superseded <tool_name> call pruned)"`
    *   The `FunctionResponse` part is replaced with: `"(superseded <tool_name> output pruned)"`
This recovers **99.9% of the byte overhead** while keeping the conversation structure and intermediate thoughts perfectly intact.

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_PRUNE_STALE_TOOLS` — Master switch. Set to `true` or `1` to enable. Defaults to `false` (disabled) on Balanced, and `true` on Aggressive and Extreme Squeeze.

---

## 📝 Example

### Before Optimization
```text
Turn 5 [Model]: Call read_file(path="main.go")
Turn 6 [User]: Response from read_file: (10,000 bytes of main.go)
...
Turn 15 [Model]: Call read_file(path="main.go")
Turn 16 [User]: Response from read_file: (10,000 bytes of main.go)
```

### After Optimization
```text
Turn 5 [Model]: Call (superseded read_file call pruned)
Turn 6 [User]: (superseded read_file output pruned)
...
Turn 15 [Model]: Call read_file(path="main.go")
Turn 16 [User]: Response from read_file: (10,000 bytes of main.go)
```
*   **Result**: The stale Turn 5 & 6 blocks are replaced with tiny 30-byte placeholders, saving 10,000 tokens while preserving the integrity of the conversation chain.
