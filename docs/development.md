# Development Guide

This document outlines how to build, test, and contribute to `cline-vertex-gw`.

---

## 1. Local Building

To build the gateway locally, ensure you have Go (version 1.22+ or 1.26 recommended) installed.

### Makefile Commands

The project includes a robust `Makefile` for standard tasks:

- **Build Binary:** Compiles the optimized binary, injecting the git-derived version string.
  ```bash
  make build
  ```
- **Run Tests:** Runs the full test suite with race-detection enabled.
  ```bash
  make test
  ```
- **Code Linting:** Checks code style and formatting.
  ```bash
  make vet
  ```

### Manual Compilation

Alternatively, you can compile manually using standard Go commands:

```bash
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
go build -ldflags "-s -w -X main.version=${VERSION}" -o cline-vertex-gw .
```

---

## 2. Testing Setup & Layout

The project places extreme emphasis on testing precision and reliability. All tests are clean and pass under `go test -race ./...`.

The codebase is organized into several types of tests:
1. **Unit Tests:** Located alongside files (e.g., `pkg/api/middleware_test.go`, `pkg/provider/vertex_test.go`). These mock upstream calls and verify handler behaviors, compression pipelines, and tool calling translations.
2. **Capability / Upstream Integration Tests:** Validate exact translations of complex formats (multimodal magic sniffing, Claude PDF document conversion, tool definitions translation).
3. **Concurrency / Race Suite:** Every helper is tested under full concurrency using Go's race detector to ensure no shared memory state issues occur under heavy multi-agent traffic.

### Running Test Commands

- **Run all tests:**
  ```bash
  go test -race -v ./...
  ```
- **Run tests with coverage analysis:**
  ```bash
  go test -coverprofile=coverage.out ./...
  go tool cover -html=coverage.out
  ```

---

## 3. GitHub Actions CI/CD Pipeline

The project integrates a comprehensive CI/CD workflow defined in `.github/workflows/ci.yml`.

### On PR / Push to `main`:
- Runs code verification (`go vet`).
- Compiles the binary across platform configurations.
- Runs the test suite with the race detector.
- Generates and uploads code coverage artifacts.
- Automatically builds multi-arch (`amd64` and `arm64`) Docker images using Docker Buildx and pushes `edge` tags to the GitHub Container Registry (GHCR).

### On Release Tag (`v*`):
- Executes all core validation tests.
- Triggers **GoReleaser** (`.goreleaser.yaml`) to cross-compile, compress, hash, and package production-ready binaries for Linux, macOS, and Windows across several architectures.
- Creates a new GitHub Release with the package binaries attached.
- Builds and publishes the official multi-architecture Docker image tagged with `latest` and the semantic version string to GHCR.
