# Native Tool & Function Calling

The gateway treats tool calling as a first-class citizen. It translates, matches, and aligns tool definitions, calls, and responses across all publishers and interfaces.

---

## 🔁 Schema Translations

Different publishers accept different schemas for declaring tools. For example, Cohere expects flattened type fields, Google expects standard JSON-Schema format inside its GenAI SDK, and OpenAI has a specific `type: "function"` nesting structure.

The gateway uses the **Google GenAI SDK's schemas** as its core internal *lingua franca* representation. 
*   **Inbound**: Inbound tool schemas (like Ollama's `tools` list or OpenAI's `tools` array) are parsed and translated into `[]*genai.Tool` inside `api/tools.go`.
*   **Outbound**: When dispatching requests, each publisher adapter translates this internal structure into their native wire format (e.g. `cohere_vertex.go` flattens schemas to parameter definitions, while `vertex.go` passes the `genai.Tool` directly).

---

## 🧪 Outbound Tool Deltas & Finish Reasons

During active generation, models emit tool calls as stream deltas or final block results. The gateway intercepts these chunks, unescapes characters if required, and formats them back into the standard expected client dialect.

### Finish Reasons Mapping
Upstream finish sentinels are converted to client-dialect standard values:

| Internal / Upstream Reason | Ollama Finish Reason | OpenAI Finish Reason |
| :--- | :--- | :--- |
| `FinishReasonToolCalls` | `stop` | `tool_calls` |
| `STOP` | `stop` | `stop` |
| `MAX_TOKENS` | `length` | `length` |
| `SAFETY` | `content_filter` | `content_filter` |

---

## ⚡ Parallel Tool Execution Support

Modern developer agents frequently emit multiple tool calls in parallel (for example, Cline might issue three parallel `read_file` calls to scan multiple code blocks in a single turn).

*   **Streaming Deltas**: Streaming adapters accumulate parallel tool call JSON segments per-index in real-time, forwarding them as structured deltas down the wire.
*   **Response Construction**: The OpenAI streaming writer (`openai_stream.go`) emits separate delta frames carrying unique `index` numbers, `id` attributes, and `function.arguments` additions. This keeps parallel execution contexts stable.

---

## 🆔 ID-Based Alignments

When clients return execution outputs to the server, they match them against prior calls. However, OpenAI and Anthropic format these exchanges differently than Google Gemini. Gemini expects a strict 400-validated count alignment of function calls and responses.

The gateway's `AlignFunctionCallsAndResponses` stage maps these lists position-by-position, resolving by **cryptographic call ID first**, and falling back to **Name matching** second. If any mismatch occurs, it synthesizes mock placeholders to guarantee that Vertex AI's gateway never throws a `400 INVALID_ARGUMENT` exception.
