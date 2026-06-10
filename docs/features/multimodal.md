# Multimodal Input Support

`cline-vertex-gw` provides robust, high-performance multimodal support on both the Ollama (`/api/chat`) and OpenAI (`/v1/chat/completions`) surface handlers.

---

## Supported Media Types

The gateway supports an expansive array of mime types across four major categories of media:

| Category | MIME Types | Supported Upstream Publishers |
|---|---|---|
| **Images** | `image/png`, `image/jpeg`, `image/webp`, `image/gif` | All publishers (Gemini, Claude, Llama, Pixtral, Qwen, etc.) |
| **PDF Documents** | `application/pdf` | Google (Gemini) & Anthropic (Claude) |
| **Audio** | `audio/wav`, `audio/mp3` (mpeg), `audio/flac`, `audio/ogg`, `audio/aac`, `audio/webm`, `audio/mp4`, `audio/m4a`, `audio/opus` | Google (Gemini) |
| **Video** | `video/mp4`, `video/webm`, `video/quicktime` (mov), `video/mpeg`, `video/x-msvideo` (avi), `video/x-flv`, `video/3gpp`, `video/ogg` | Google (Gemini) |

---

## Core Media Systems

### 1. Magic-Bytes Sniffing (`sniffMediaMIME`)
On the Ollama surface, clients send files inside a flat `images` array containing base64-encoded strings, without providing explicit MIME types. 

To determine how to translate these parts, the gateway employs an internal **magic-bytes sniffer** (`sniffMediaMIME`). It parses the first few bytes of the decoded file header to accurately resolve standard formats (including PNG, JPEG, GIF, WebP, PDF, WAV, MP3, MP4, FLAC, Ogg, and AVI).

### 2. Upstream Capability Validation (`publisherSupportsMIME`)
Different model publishers hosted on Vertex AI have varying native restrictions on multimodal inputs (e.g. Gemini supports everything; Claude supports images and PDFs; Meta/Mistral/Qwen support images only).

To prevent confusing, mid-stream failures, the gateway validates request attachments at the gate using `publisherSupportsMIME`. Sending an unsupported file type to a model (e.g., a PDF to a text-only Llama model) returns a clean, parseable `400 Bad Request` explaining exactly what is wrong and recommending alternative model families:

```json
{
  "error": {
    "message": "model \"llama-3.3-70b-instruct-maas\" (publisher=\"meta\") does not support image inputs on Vertex AI; use a vision-capable model instead — e.g. gemini-2.0-flash, claude-3-5-sonnet, llama-3.2-90b-vision-instruct-maas, or pixtral-12b",
    "type": "invalid_request_error",
    "code": "model"
  }
}
```

### 3. Native Anthropic PDF Translation
Anthropic Claude models support PDF documents natively. However, they expect PDF attachments to be structured as specific inline `document` blocks. The gateway automatically translates PDF inputs into Claude's native `document` blocks when routing requests to Claude on Vertex.

### 4. Polymorphic `input_audio` Support
For advanced OpenAI clients, the gateway fully supports the standard, polymorphic `input_audio` schema (with custom `format` and `data` strings), automatically converting them into standard Vertex AI audio inline data parts.

---

## Image Deduplication (`dedup`)

Because agent environments like Cline take screenshots on every single turn, a long conversation often carries near-identical, massive image files across dozens of turns, leading to exponential cost and performance degradation.

To mitigate this, `cline-vertex-gw` implements an automated **Image Deduplication** optimizer:
- The gateway hashes incoming image bytes (SHA-256 scoped by role and MIME type).
- Subsequent identical images are replaced with a lightweight text-part placeholder pointing backward to the turn where the image was first introduced.
- This ensures that sending the same 800 KB screenshot 5 times across a session uploads ~800 KB of data instead of 4 MB, dramatically reducing upload bandwidth and latencies.

---

## Limits & Safety Constraints

- **SSRF Mitigation:** The gateway only accepts inline, base64-encoded data strings. It **never** makes outbound HTTP/HTTPS requests to fetch user-supplied URLs. This prevents Server-Side Request Forgery (SSRF) vulnerabilities entirely.
- **Size Caps:** To protect gateway and model performance, per-media size limits are enforced on decoded file bytes:
  - `GW_MAX_MEDIA_BYTES_PER_PART` (default: 10 MiB) — Max size of any single file.
  - `GW_MAX_MEDIA_BYTES_PER_REQUEST` (default: 32 MiB) — Max aggregate size of all media in a single request.
- **Context Filtering:** Media attached to `system`, `developer`, or `tool` messages are silently filtered out since no upstream publishers support them.
- **Surface Gating:** The single-turn `/api/generate` endpoint does not support multimodal inputs. Use `/api/chat` or `/v1/chat/completions` for multimodal tasks.

---

## Smoke Test Example

You can smoke test the gateway's vision capability against Gemini using `curl`:

```bash
IMG_B64=$(base64 -w0 < screenshot.png)

curl -sS http://127.0.0.1:11434/v1/chat/completions \
  -H "Authorization: Bearer $GATEWAY_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"gemini-2.0-flash\",
    \"stream\": false,
    \"messages\": [{
      \"role\": \"user\",
      \"content\": [
        {\"type\": \"text\", \"text\": \"describe this image in one sentence\"},
        {\"type\": \"image_url\", \"image_url\": {\"url\": \"data:image/png;base64,${IMG_B64}\"}}
      ]
    }]
  }"
```
