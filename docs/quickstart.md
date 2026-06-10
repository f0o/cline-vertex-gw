# Quick Start Guide

This guide gets you up and running with `cline-vertex-gw` in just a few minutes.

---

## Prerequisites

Before starting, ensure you have the following resources and permissions:

1. **Google Cloud Project:** An active GCP project with the Vertex AI API enabled.
2. **Authentication (ADC):** The gateway uses Google's standard Application Default Credentials (ADC) to authenticate with Google Cloud.
   - For local development, install the `gcloud` CLI and run:
     ```bash
     gcloud auth application-default login
     ```
3. **Core Environment Variables:** Set your Google Cloud Project ID and location/region:
   ```bash
   export GOOGLE_CLOUD_PROJECT="your-gcp-project-id"
   export GOOGLE_CLOUD_LOCATION="us-central1" # Or Europe/Asia locations
   ```

---

## 1. Run the Gateway

The gateway listens on port `11434` (Ollama's standard port) by default. Choose one of the following methods to run the proxy.

### Option A: Run via Docker
To run locally, share your local Google credentials with the container:

```bash
docker run -d \
  -p 11434:11434 \
  -v ~/.config/gcloud:/root/.config/gcloud:ro \
  -e GOOGLE_CLOUD_PROJECT="$GOOGLE_CLOUD_PROJECT" \
  -e GOOGLE_CLOUD_LOCATION="$GOOGLE_CLOUD_LOCATION" \
  ghcr.io/f0o/cline-vertex-gw:latest
```

*Note:* Mounting `~/.config/gcloud` shares your local ADC credentials with the container. In production environments (like Cloud Run or GKE), attach an IAM service account to the execution resource instead, and omit the volume mount.

### Option B: Build & Run from Source
Ensure you have Go (1.22+ or 1.26 recommended) installed:

```bash
# Clone the repository
git clone https://github.com/f0o/cline-vertex-gw.git
cd cline-vertex-gw

# Compile the optimized binary
make build

# Launch the server
./cline-vertex-gw
```

---

## 2. Quick Smoke Tests

Verify the gateway is running correctly by querying its endpoints locally:

### A. Verify Model Discovery (Ollama Dialect)
Query the `/api/tags` endpoint to discover supported models formatted as local Ollama tags:

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

### B. Verify Chat Streaming (Ollama Dialect)
Execute a streaming chat completion over the Ollama interface:

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

### C. Verify OpenAI Dialect
Submit a non-streaming chat request to the OpenAI-compatible endpoint:

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

To ensure the most robust, high-performance experience with real-time stream metrics and delta-by-delta streaming tool-calling outputs, it is **strongly recommended** to connect your clients utilizing the **OpenAI-Compatible** provider configuration over the Ollama compatibility fallback.

### Connecting VS Code Cline

#### Method A: OpenAI Compatible Provider (Recommended & Superior)
Using the OpenAI compatible provider unlocks real-time tool calling streaming and detailed usage metrics inside Cline.

1. Open Cline's settings pane inside VS Code.
2. Select **OpenAI Compatible** as the API Provider.
3. Set the **Base URL** field to: `http://localhost:11434/v1`
4. Enter any non-empty string as the **API Key** (if you have configured `GATEWAY_AUTH_TOKEN` on the gateway, enter the exact token value here).
5. Set your desired **Model ID** (e.g. `claude-3-5-sonnet`, `gemini-2.0-flash`, etc.).
6. Enter custom model details, or click save. Enjoy lightning-fast, real-time tool stream updates!

#### Method B: Ollama Provider (Compatibility Fallback)
Maintained as a drop-in local discovery option:

1. Open Cline's settings in VS Code.
2. Select **Ollama** as the API Provider.
3. In the **Ollama Base URL** field, enter: `http://localhost:11434`
4. The **Model** picker will automatically populate with all available models (Gemini, Claude, Llama, Mistral, etc.) discovered from Vertex AI! Select your desired model.

---

### Connecting Continue

To configure Continue (`config.json`), we recommend using the OpenAI provider setup to guarantee optimal tool usage and metrics handling:

#### Recommended Configuration (OpenAI Provider):
```json
{
  "models": [
    {
      "title": "Claude 3.5 Sonnet (Vertex)",
      "provider": "openai",
      "model": "claude-3-5-sonnet",
      "apiBase": "http://localhost:11434/v1",
      "apiKey": "placeholder"
    }
  ]
}
```

#### Fallback Configuration (Ollama Provider):
```json
{
  "models": [
    {
      "title": "Claude 3.5 Sonnet (Vertex)",
      "provider": "ollama",
      "model": "claude-3-5-sonnet",
      "apiBase": "http://localhost:11434"
    }
  ]
}
```
