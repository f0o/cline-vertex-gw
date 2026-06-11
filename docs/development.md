# Development & Contributing

Contributions to the **Cline Vertex AI Gateway** are welcome. The project is structured as a standard Go module with explicit tasks and a streamlined compilation and testing setup.

---

## 🛠️ Makefile Tasks

The project uses a standard `Makefile` to automate common development workflows.

```text
Usage: make [target]

Targets:
  build             Compile the gateway for the host platform
  test              Run all unit tests in the workspace (excluding race and integration)
  test-race         Run all unit tests with Go's race detector active
  vet               Execute go vet, staticcheck, and security scanners
  docker-build      Build the multi-stage Docker container locally
  clean             Remove compiled binaries and test coverage profiles
```

For example, to run the static checkers, code formatting tests, and unit tests:

```bash
make vet && make test-race
```

---

## 🧪 Testing Structure

The workspace has excellent test coverage, specifically targeting streaming boundaries, translation structures, unescaping loops, and optimization pipelines.

Tests are written using Go's standard `testing` package.

### Key Test Categories & Locations
*   **Pipeline Stages**: Covered in `pkg/pipeline/*_test.go`. These files verify that the compressors do not mutate state, respect safety invariants (e.g. not pruning the first turn or active workspace modifications), and calculate sizes correctly.
*   **Unescaping Streaming**: Covered in `pkg/api/unescape_test.go` and `pkg/pipeline/pipeline_test.go`. These tests mock streaming chunk splits, verifying that Cline XML blocks are unescaped cleanly even if split mid-chunk.
*   **Upstream Translators & Retries**: Covered in `pkg/provider/*_test.go` and `pkg/api/retry_test.go`. These tests assert that adapters (like Anthropic and Cohere) produce the correct JSON payloads and that overloaded errors are retried.
*   **Cost Estimation & Scraping**: Covered in `pkg/provider/pricing_test.go` and `pkg/api/metrics_test.go`. These tests verify that SKU-key resolution maps unreleased versions correctly and aggregates Prometheus counts accurately.

Run all tests with:
```bash
go test -race ./...
```

---

## 📦 Multi-Architecture Builds

The project uses **GoReleaser** to compile and package binaries across multiple platforms and architectures.

To execute a local dry-run release and output platform-specific binaries into `dist/`:

```bash
goreleaser release --snapshot --clean
```

Binaries will be generated for:
*   **Linux**: `amd64`, `arm64`, `386`
*   **macOS**: `amd64`, `arm64` (Universal Apple Silicon support)
*   **Windows**: `amd64`, `arm64`
