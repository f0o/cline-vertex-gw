# Multimodal Input Processing

The gateway implements comprehensive, high-fidelity multimodal capabilities across both Ollama and OpenAI API interfaces, allowing developer agents to process visual, auditory, textual, and video assets.

---

## 🖼️ Supported Media Formats & Sniffer

The gateway features a robust magic-bytes sniffer (`sniffMediaMIME`) that automatically detects media MIME-types on the raw binary chunks submitted by Ollama clients (which typically lack explicit MIME-type labels on their native `images` string slice).

### Supported Formats & MIME Mapping
*   **Images**: PNG (`image/png`), JPEG (`image/jpeg`), WEBP (`image/webp`), GIF (`image/gif`).
*   **Audio**: WAV (`audio/wav`), MP3 (`audio/mp3`), FLAC (`audio/flac`), Ogg (`audio/ogg`), AAC (`audio/aac`), Opus (`audio/opus`), M4A (`audio/m4a`).
*   **Video**: MP4 (`video/mp4`), WebM (`video/webm`), MOV (`video/mov`), MPEG (`video/mpeg`), AVI (`video/avi`), FLV (`video/flv`), 3GPP (`video/3gpp`).
*   **Text Documents**: PDF (`application/pdf`).

---

## 🚫 Publisher Gating Matrix

Different foundation models on Vertex AI have varying physical capability limits (for example, Meta Llama models support vision only, while Google Gemini supports audio, video, documents, and images). 

The gateway enforces **Upstream Gating** under the helper `publisherSupportsMIME` to validate requests *before* initiating network connections:

| Publisher Namespace | Images | Audio | Video | PDF Documents |
| :--- | :---: | :---: | :---: | :---: |
| `google` (Gemini) | **Yes** | **Yes** | **Yes** | **Yes** |
| `anthropic` (Claude) | **Yes** | No | No | **Yes** |
| `meta` (Llama) | **Yes** (Only `*vision*`) | No | No | No |
| `mistralai` (Pixtral) | **Yes** | No | No | No |
| `qwen` (Qwen-VL) | **Yes** | No | No | No |
| `nvidia` | **Yes** | No | No | No |
| *Others* (DeepSeek, etc.) | No | No | No | No |

If an unsupported asset format is passed to a model, the gateway immediately aborts the stream and returns a descriptive `400 Bad Request` explaining exactly which model you should select instead.

---

## 📑 Claude Document Block Mapping

While Google Gemini accepts PDFs directly as inline data blobs within standard message turns, Anthropic's Messages API uses a distinct `document` block structure for PDF processing.

The gateway's Anthropic adapter automatically intercepts `application/pdf` inline data parts and transforms them into native Claude messages:

```json
{
  "type": "document",
  "source": {
    "type": "base64",
    "media_type": "application/pdf",
    "data": "[Base64 Payload]"
  }
}
```

---

## 🛡️ Decoupled Size Guards

To prevent memory bloat or Server Side Request Forgery (SSRF) denial-of-service, the gateway implements strict decoupled byte ceilings. It validates the size of decoded media payloads prior to running upstream connections:

*   **Single Image Part**: Configured via `GW_MAX_IMAGE_BYTES_PER_PART` (default `10 MiB`).
*   **Cumulative Image Request**: Configured via `GW_MAX_IMAGE_BYTES_PER_REQUEST` (default `32 MiB`).
*   **Single Media Part (Audio/Video/PDF)**: Configured via `GW_MAX_MEDIA_BYTES_PER_PART` (default `50 MiB`).
*   **Cumulative Media Request**: Configured via `GW_MAX_MEDIA_BYTES_PER_REQUEST` (default `100 MiB`).
