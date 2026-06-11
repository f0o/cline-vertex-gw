# 5. Function Call Alignment

The **Function Call Alignment** stage is positioned at step `5` of the optimization pipeline. It inspects the conversation history and enforces a strict, order-preserving alignment between the model's function calls and the client's function responses to guarantee upstream compatibility with Google Cloud.

---

## 🔍 Why It Matters

Google Cloud's **Vertex AI Gemini API** implements exceptionally strict validation checks on active tool calling sessions:
1.  **Count Alignment**: If an assistant/model turn contains $N$ separate tool calls (`FunctionCall` parts), the subsequent user turn **must contain exactly $N$ matching tool response parts** (`FunctionResponse` parts).
2.  **Order Invariant**: The responses must reside in the exact same index positions as their corresponding calls.
3.  **The Penalty**: If there is any mismatch (e.g. Cline's parser crashed and failed to return output for one of three parallel tool calls, or returned them in a scrambled index order), Gemini's API gateway instantly rejects the entire request with a fatal **`HTTP 400 INVALID_ARGUMENT`** exception, breaking the active developer session.

The **Function Call Alignment** stage provides a robust, self-healing compatibility layer. It dynamically parses and reconstructs tool turns before sending them upstream, ensuring Gemini always receives perfect, index-aligned payloads.

---

## ⚙️ How It Works

This stage is implemented in `pkg/pipeline/align.go` and executes as follows:

### 1. Model Turn Scanning
The engine walks through the conversation history. When it detects a `model` (or `assistant`) turn containing one or more `FunctionCall` parts (or placeholder tombstones left by stale tool pruning):
*   It looks ahead to the immediately subsequent `user` turn.
*   **Role Alternation**: If the subsequent turn is not a `user` turn (or is missing entirely), it continues, as Vertex AI's gateway will handle general conversational desyncs separately.

### 2. Match & Queue Alignment
The engine collects all available `FunctionResponse` parts inside the subsequent `user` turn:
*   **Pairing by ID**: It attempts to pair responses to calls using their cryptographic call ID (`fc.ID == fr.ID`). This is highly robust because modern APIs (OpenAI/Anthropic MaaS) carry unique transaction IDs.
*   **Pairing by Name**: If IDs are missing or blank, the engine falls back to matching by tool name (`fc.Name == fr.Name`).

### 3. Dummy Synthesis & Dropping
The engine constructs a new, aligned list of parts for the subsequent `user` turn, matching the model's calls index-for-index:
*   **Successful Matches**: Appended to the turn at the corresponding index, and removed from the available pool.
*   **Missing Responses (Dummy Synthesis)**: If a tool call has no corresponding client response, the engine **synthesizes a dummy response** to satisfy Gemini's count check, returning:
    `{"output": "omitted by client"}`
*   **Extra/Unsolicited Responses**: Any extra responses in the pool that do not correspond to any call in the model turn are **silently dropped**, preventing count overruns.
*   **Text Part Protection**: All standard, non-tool parts in the user turn (like conversational text or environment details) are placed at the very front of the turn, keeping them completely safe from deletion.

---

## 🎛️ Configuration Parameters

This stage is **unconditional** and does not have an off-switch, as it is a strict, load-bearing requirement for Google Gemini models. It is only executed when the upstream target is a Gemini model (`isGemini == true`).

---

## 📝 Example

### Before Optimization
```text
Turn 8 [Model]: (Emitted 2 parallel tool calls)
  - Call 1: read_file(path="main.go")
  - Call 2: execute_command(command="ls")

Turn 9 [User]: (Cline returns only 1 response due to command terminal desync)
  - Response for Call 1: {"content": "package main..."}
```

### After Optimization
```text
Turn 8 [Model]: (Emitted 2 parallel tool calls)
  - Call 1: read_file(path="main.go")
  - Call 2: execute_command(command="ls")

Turn 9 [User]: (Gateway aligns index count to exactly 2)
  - Response for Call 1: {"content": "package main..."}
  - Response for Call 2: {"output": "omitted by client"}  <-- Synthesized!
```
*   **Result**: The index count of responses is perfectly aligned to match the calls, preventing Gemini's API gateway from throwing a fatal 400 Bad Request exception.
