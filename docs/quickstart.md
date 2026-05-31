# Quick Start

This guide gets you up and running with `cline-vertex-gw` in just a few minutes.

## Prerequisites

1. **Google Cloud Project:** You need a GCP project with the Vertex AI API enabled and active billing.
2. **Authentication:** The gateway uses Google Application Default Credentials (ADC) to authenticate with GCP. Ensure you have authenticated locally or your runtime environment has appropriate IAM permissions.
   - For local development, install the `gcloud` CLI and run:
     ```bash
     gcloud auth application-default login
     ```
3. **Environment Variables:** Set your Google Cloud Project ID and region:
   ```bash
   export GOOGLE_CLOUD_PROJECT="your-gcp-project-id"
   export GOOGLE_CLOUD_LOCATION="us-central1" # or other supported region
   ```

---

## 1. Run the Gateway

Choose one of the following methods to run the gateway. By default, it listens on `127.0.0.1:11434` (Ollama's standard port).

### Option A: Run via Docker

The official multi-architecture Docker images are published to the GitHub Container Registry (GHCR):

```bash
docker run -d \
  -p 11434:11434 \
  -v ~/.config/gcloud:/root/.config/gcloud:ro \
  -e GOOGLE_CLOUD_PROJECT="$GOOGLE_CLOUD_PROJECT" \
  -e GOOGLE_CLOUD_LOCATION="$GOOGLE_CLOUD_LOCATION" \
  ghcr.io/f0o/cline-vertex-gw:latest
```

*Note:* Mounting `~/.config/gcloud` shares your local Application Default Credentials with the container. In production, run the gateway on GCP (Cloud Run, GKE, GCE) with an IAM service account attached and omit the volume mount.

### Option B: Pre-built Binaries

Download the appropriate binary for your platform and architecture from the GitHub Releases page.

To run it:
```bash
./cline-vertex-gw
```

### Option C: Build & Run from Source

If you have Go installed (version 1.22+ or 1.26 recommended):

```bash
git clone https://github.com/f0o/cline-vertex-gw.git
cd cline-vertex-gw
make build
./cline-vertex-gw
```

---

## 2. Quick Smoke Test

Once the gateway is running, verify it with `curl`:

### Verify Model Discovery (Ollama Surface)

Query the `/api/tags` endpoint. This returns enabled Vertex AI models formatted as local Ollama tags:

```bash
curl -s http://127.0.0.1:11434/api/tags | json_pp
```

*Expected response structure:*
```json
{
   "models" : [
      {
         "name" : "gemini-2.0-flash",
         "model" : "gemini-2.0-flash",
         "details" : {
            "family" : "gemini",
            "format" : "gguf"
         }
      },
      {
         "name" : "claude-3-5-sonnet",
         "model" : "claude-3-5-sonnet",
         "details" : {
            "family" : "claude",
            "format" : "gguf"
         }
      }
   ]
}
```

### Verify Chat Streaming (Ollama Surface)

```bash
curl -i http://127.0.0.1:11434/api/chat -d '{
  "model": "gemini-2.0-flash",
  "messages": [
    {"role": "user", "content": "Hello! Respond in exactly three words."}
  ],
  "stream": true
}'
```

This returns a stream of JSON objects (NDJSON) representing response tokens, followed by a final metrics frame containing token usage.

### Verify OpenAI-compatible Endpoint

```bash
curl -i http://127.0.0.1:11434/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet",
    "messages": [
      {"role": "user", "content": "Hello! Respond in exactly three words."}
    ],
    "stream": false
  }'
```

---

## 3. Connecting Clients

### Connecting Cline

Cline is designed to interact natively with Ollama. Connecting Cline to your Vertex AI-backed gateway is straightforward:

1. Open Cline's settings in VS Code.
2. Select **Ollama** as the API Provider.
3. In the **Ollama Base URL** field, enter: `http://localhost:11434`
4. The **Model** picker will automatically populate with all available models (Gemini, Claude, Llama, Mistral, etc.) discovered from Vertex AI! Select your desired model (e.g., `claude-3-5-sonnet` or `gemini-2.5-pro`).
5. (Optional) If you configured a bearer token on the gateway (`GATEWAY_AUTH_TOKEN`), set it in the client configuration or use the OpenAI-compatible provider instead.

### Connecting Other Clients

- **LiteLLM / LangChain / OpenAI SDKs:** Point the base URL to `http://localhost:11434/v1` and use standard models. If `GATEWAY_AUTH_TOKEN` is configured, pass it in the standard `Authorization: Bearer <token>` header.
- **Continue VS Code Extension:** Configure Continue to use either the Ollama provider or the OpenAI provider.
