# syntax=docker/dockerfile:1.7

# ---- build stage ----------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Cache dependencies separately from source for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION is injected at build time by CI / `docker build --build-arg`. It
# ends up baked into the binary via `-X main.version=...` so /healthz, the
# /version endpoint, and the startup log line all report something useful.
ARG VERSION=dev

# CGO disabled => fully static binary, suitable for distroless/scratch.
# -trimpath strips local paths from the binary for reproducibility.
# -ldflags strips DWARF/symtab AND injects the version.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/cline-vertex-gw .

# ---- runtime stage --------------------------------------------------------
# distroless/static is ~2 MB, has no shell, no package manager, no libc, and
# ships the CA bundle needed for outbound TLS to Vertex AI.
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
LABEL org.opencontainers.image.title="cline-vertex-gw" \
      org.opencontainers.image.description="Ollama- and OpenAI-compatible gateway for Google Vertex AI" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.source="https://go.f0o.dev/cline-vertex-gw" \
      org.opencontainers.image.version="${VERSION}"

# Defaults — override at run time with `-e` / `--env`.
#
# BIND_ADDR=0.0.0.0 inside containers (vs. the binary's 127.0.0.1 default
# when run on the host) because a container's loopback is unreachable from
# anywhere else. Operators are expected to put the container behind a
# trusted reverse proxy / Cloud Run / k8s service that handles ingress.
ENV PORT=11434 \
    BIND_ADDR=0.0.0.0 \
    MAX_REQUEST_MB=16 \
    READ_HEADER_TIMEOUT_SEC=10 \
    IDLE_TIMEOUT_SEC=120 \
    WRITE_TIMEOUT_SEC=0 \
    SHUTDOWN_TIMEOUT_SEC=30 \
    LOG_FORMAT=json \
    LOG_LEVEL=info

EXPOSE 11434

COPY --from=build /out/cline-vertex-gw /usr/local/bin/cline-vertex-gw

USER nonroot:nonroot

# No HEALTHCHECK directive: distroless ships no shell or curl, so we can't
# run a probe binary in-image. The /healthz endpoint is the documented
# liveness probe for external orchestrators (Cloud Run, k8s livenessProbe,
# docker-compose with an external curl sidecar, etc.).

ENTRYPOINT ["/usr/local/bin/cline-vertex-gw"]