# Deployment & Quickstart

Getting started with the **Cline Vertex AI Gateway** is a straightforward process. The gateway relies on standard Google Cloud credential configurations, compiles into a lightweight binary, and can be run locally, via Docker, or deployed as a shared service.

---

## 🔐 1. Google Cloud Authentication

The gateway is built on top of the Google Cloud Go SDK, which natively supports **Google Application Default Credentials (ADC)**. This means it can safely authenticate against Vertex AI without requiring hardcoded credential strings or complex configurations.

Choose **one** of the following methods to authenticate:

### Method A: Local Developer (Recommended)
If running on a local development machine, run the following command to generate standard local ADC files:

```bash
gcloud auth application-default login
```

This commands writes a secure token file to:
*   **macOS/Linux**: `~/.config/gcloud/application_default_credentials.json`
*   **Windows**: `%APPDATA%\gcloud\application_default_credentials.json`

The gateway will find and read this file automatically.

### Method B: Service Account Key (Server Deployment)
If deploying onto an external server or cloud VM:
1.  Create a Service Account in the [Google Cloud Console](https://console.cloud.google.com).
2.  Assign the Service Account the **Vertex AI User** role (`roles/aiplatform.user`) and, if using cost-estimation metrics, **Billing Account Viewer** (`roles/billing.viewer`).
3.  Generate and download a JSON-formatted Service Account Key.
4.  Expose the path to this key via the standard Google environment variable:
    ```bash
    export GOOGLE_APPLICATION_CREDENTIALS="/path/to/your/service-account-key.json"
    ```

---

## 🏃 2. Running the Gateway

Choose how you want to compile and run the gateway server:

### Method A: Pre-built Docker Container (easiest)
Run the gateway in a lightweight, secure container. We map port `11434` (Ollama's default port) and mount the local GCP credential folder so the container can authenticate:

```bash
docker run -d \
  -p 11434:11434 \
  -e GOOGLE_CLOUD_PROJECT="your-gcp-project-id" \
  -e GOOGLE_CLOUD_LOCATION="us-central1" \
  -e GW_PROFILE="balanced" \
  -v ~/.config/gcloud:/home/nonroot/.config/gcloud:ro \
  ghcr.io/f0o/cline-vertex-gw:latest
```

### Method B: Native Compilation (From Source)
Ensure you have Go 1.22+ installed, then compile and run the binary natively:

```bash
# Clone the repository
git clone https://github.com/f0o/cline-vertex-gw.git
cd cline-vertex-gw

# Build the executable
make build

# Export required project parameters
export GOOGLE_CLOUD_PROJECT="your-gcp-project-id"
export GOOGLE_CLOUD_LOCATION="us-central1"
export GW_PROFILE="balanced"

# Execute the binary
./cline-vertex-gw
```

---

## 🔌 3. Client Integrations

Once the gateway is running on `http://localhost:11434` (or your customized `BIND_ADDR`), configure your client tools to use it:

### Integration A: Cline (Ollama Protocol)
Cline can natively discover all available models of all enabled publishers over the Ollama `/api/tags` route.

1.  In the VS Code sidebar, open **Cline**.
2.  Click the **Settings** (Gear Icon) in the top-right.
3.  Set **Provider** to `Ollama`.
4.  Keep the **Ollama URL** at its default value: `http://localhost:11434`.
5.  Click the **Model** dropdown. It will automatically load the model list from the gateway. 
6.  Select your preferred model (e.g. `google/gemini-2.5-pro` or `anthropic/claude-3-5-sonnet-v2`) and close the settings to begin.

---

### Integration B: OpenAI-Compatible Clients
If using standard OpenAI-compatible development tools (such as LiteLLM, prompt foo, or the official Python `openai` SDK), connect using the OpenAI client surface.

#### Example: Python SDK
Configure the OpenAI Python client to point directly at the gateway's `/v1` surface.

```python
import openai

client = openai.OpenAI(
    base_url="http://localhost:11434/v1",
    api_key="your-gateway-auth-token-or-dummy-string"  # Must match GATEWAY_AUTH_TOKEN if configured
)

completion = client.chat.completions.create(
    model="google/gemini-2.5-pro",
    messages=[
        {"role": "system", "content": "You are a concise, helpful assistant."},
        {"role": "user", "content": "Hello, world!"}
    ],
    stream=True
)

for chunk in completion:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
```
