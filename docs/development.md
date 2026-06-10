# Development Guide

This guide outlines the compilation, testing, and CI/CD operations for contributing to `cline-vertex-gw`.

---

## 1. Local Building

To build and compile the gateway locally, ensure you have Go (Go 1.22+ or 1.26 recommended) installed.

### Makefile Operations
The project contains a pre-configured `Makefile` to simplify common actions:

- **Compile Optimized Binary:**
  ```bash
  make build
  ```
- **Execute Linting and Vet checks:**
  ```bash
  make vet
  ```
- **Run Full Test Suite (Race Detection Enabled):**
  ```bash
  make test
  ```

### Manual Compilation
To compile the binary manually while injecting Git release tags and version metadata, run:

```bash
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
go build -ldflags "-s -w -X main.version=${VERSION}" -o cline-vertex-gw .
```

---

## 2. Testing Framework & Layout

The project enforces extreme reliability through structured testing. All tests are thread-safe and must run cleanly with the `-race` detector enabled.

### Codebase Test Types
1. **Unit Tests:** Placed adjacent to implementation source files (e.g. `pkg/api/middleware_test.go`, `pkg/provider/vertex_test.go`). These mock upstream APIs and verify edge cases, configurations, and handler responses.
2. **Translation & Pipeline Tests:** Validate complex conversion loops, including multimodal magic-bytes sniffing, Anthropic PDF translation, and prompt optimization compression stages.
3. **Concurrency & Thread Safety Suite:** Heavily stress-tests shared utility structs (like tags cache and token ratelimit buffers) under parallel load.

### Running Tests

```bash
# Run the complete test suite with race-detection
go test -race -v ./...

# Run tests and output HTML coverage reports
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

---

## 3. GitHub Actions CI/CD Pipelines

The repository features an automated workflow configured in `.github/workflows/ci.yml`.

### A. Actions on Pull Request or Push to `main`
- Runs static analysis and structural code checks (`go vet`).
- Executes the entire test suite with the Go race-detector active.
- Compiles the application across multiple standard OS configurations.
- Performs multi-platform Docker builds (arm64 & amd64) and publishes bleeding-edge containers tagged with `edge` to the GitHub Container Registry (GHCR).

### B. Actions on Semantic Release Tags (`v*`)
- Executes strict release pre-validations and tests.
- Triggers **GoReleaser** to cross-compile, compress, hash, and publish production binaries (for Linux, macOS, and Windows) as a new GitHub Release.
- Compiles and publishes multi-architecture Docker containers tagged with `latest` and the precise release version to GHCR.
