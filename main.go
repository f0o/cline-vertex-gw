package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.f0o.dev/cline-vertex-gw/api"
	"go.f0o.dev/cline-vertex-gw/provider"
)

// version is overridden at build time via:
//
//	go build -ldflags "-X main.version=v1.2.3"
//
// CI populates it from `git describe --tags --always --dirty`. The startup
// log line and /healthz response include it so operators can confirm which
// build is running.
var version = "dev"

// Environment variable names.
const (
	envPort       = "PORT"
	envBind       = "BIND_ADDR" // defaults to 127.0.0.1 (loopback only) — set to "0.0.0.0" or a specific interface to expose
	envProject    = "GOOGLE_CLOUD_PROJECT"
	envLocation   = "GOOGLE_CLOUD_LOCATION"
	envAuthToken  = "GATEWAY_AUTH_TOKEN" // optional bearer token; if empty, server runs unauthenticated
	envMaxBodyMB  = "MAX_REQUEST_MB"     // optional; default 16
	envReadHeader = "READ_HEADER_TIMEOUT_SEC"
	envIdle       = "IDLE_TIMEOUT_SEC"
	envWrite      = "WRITE_TIMEOUT_SEC" // 0 disables (needed for long streams); default 0
	envShutdown   = "SHUTDOWN_TIMEOUT_SEC"
	envLogFormat  = "LOG_FORMAT" // "json" (default) or "text"
	envLogLevel   = "LOG_LEVEL"  // "debug"|"info"|"warn"|"error"; default "info"
)

// configureLogging installs a slog handler as the program-wide default and
// also reroutes the legacy `log` package through it. This means every
// existing log.Printf call in the codebase (~25 sites) emits structured
// JSON automatically — no per-call-site changes needed.
//
// LOG_FORMAT=text falls back to the human-readable text handler for local
// development; default is JSON for production / aggregator-friendly use.
func configureLogging() {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envLogLevel))) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envLogFormat))) {
	case "text":
		h = slog.NewTextHandler(os.Stderr, opts)
	default: // "json", "", or anything else
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	logger := slog.New(h)
	slog.SetDefault(logger)
	// Bridge stdlib log → slog so existing log.Printf calls become structured.
	// They land at INFO with msg=<formatted line>. Caller info is stripped
	// (the original log call site isn't worth the extra runtime cost).
	log.SetFlags(0)
	log.SetOutput(&slogBridge{logger: logger})
}

// slogBridge wraps a slog.Logger as an io.Writer so log.Printf output flows
// through the structured handler. Each Write becomes one INFO record.
type slogBridge struct{ logger *slog.Logger }

func (b *slogBridge) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	b.logger.Info(msg)
	return len(p), nil
}

func mustEnvInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Printf("invalid %s=%q; using default %d", name, v, def)
		return def
	}
	return n
}

// isLoopback reports whether the bind address restricts the listener to the
// local machine. Used to gate the "unauthenticated AND exposed" warning.
func isLoopback(bind string) bool {
	switch bind {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

func main() {
	configureLogging()
	port := os.Getenv(envPort)
	if port == "" {
		port = "11434" // Default Ollama port
	}
	// Loopback-by-default: a single-tenant gateway holding GCP credentials
	// should NOT be reachable from the network unless the operator explicitly
	// opts in. Set BIND_ADDR=0.0.0.0 (or a specific interface) to expose,
	// typically when fronted by a reverse proxy that terminates TLS and auth.
	bind, bindSet := os.LookupEnv(envBind)
	if !bindSet {
		bind = "127.0.0.1"
	}

	projectID := os.Getenv(envProject)
	location := os.Getenv(envLocation)

	if projectID == "" || location == "" {
		log.Printf("WARNING: %s or %s not set; Vertex AI calls will fail until configured.",
			envProject, envLocation)
	}

	authToken := strings.TrimSpace(os.Getenv(envAuthToken))
	if authToken == "" {
		log.Printf("SECURITY WARNING: %s is not set; the gateway is running UNAUTHENTICATED. "+
			"Set %s to require `Authorization: Bearer <token>` on all /api/* and /v1/* requests.",
			envAuthToken, envAuthToken)
	}
	// Loud warning if the operator opted to bind non-loopback without auth —
	// this is the single most dangerous misconfiguration possible.
	if authToken == "" && !isLoopback(bind) {
		log.Printf("SECURITY WARNING: bound to %q WITHOUT %s set — anyone reachable on "+
			"that interface can spend your Vertex AI quota. Either set %s or "+
			"unset/set %s=127.0.0.1 to restrict to loopback.",
			bind, envAuthToken, envAuthToken, envBind)
	}

	maxBodyMB := mustEnvInt(envMaxBodyMB, 16)
	readHeaderTimeout := time.Duration(mustEnvInt(envReadHeader, 10)) * time.Second
	idleTimeout := time.Duration(mustEnvInt(envIdle, 120)) * time.Second
	// WriteTimeout MUST default to 0 (no limit): completions stream for many
	// seconds; a fixed write deadline would truncate them. Operators can opt in
	// if they front this with a load-balancer that enforces its own timeout.
	writeTimeout := time.Duration(mustEnvInt(envWrite, 0)) * time.Second
	shutdownTimeout := time.Duration(mustEnvInt(envShutdown, 30)) * time.Second

	// Root context cancelled on SIGINT/SIGTERM so in-flight upstream calls can
	// exit promptly via context propagation.
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var vertexClient *provider.VertexClient
	if projectID != "" && location != "" {
		vc, err := provider.NewVertexClient(rootCtx, projectID, location)
		if err != nil {
			log.Fatalf("Failed to initialize Vertex AI client: %v", err)
		}
		vertexClient = vc
		defer vertexClient.Close()
	}

	// Publish the build version so /healthz and structured-log preambles can
	// surface it. Done before SetupRoutes so the health handler sees it.
	api.SetBuildVersion(version)

	mux := api.SetupRoutes(vertexClient)

	// Wrap handlers with: recover-from-panic -> auth -> body-size-limit.
	// Order matters: panic recovery is the outermost layer so any later
	// middleware that panics still returns a clean 500. Auth runs before the
	// body limiter so unauthorized clients can't consume the body budget.
	handler := api.RecoverMiddleware(
		api.AuthMiddleware(authToken,
			api.BodyLimitMiddleware(int64(maxBodyMB)*1024*1024, mux)))

	addr := fmt.Sprintf("%s:%s", bind, port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		WriteTimeout:      writeTimeout,
		MaxHeaderBytes:    1 << 20, // 1 MiB
		BaseContext:       func(_ net.Listener) context.Context { return rootCtx },
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("Starting Cline Vertex Gateway version=%s on %s (auth=%v, max_body=%dMB, write_timeout=%v, loopback=%v)",
			version, addr, authToken != "", maxBodyMB, writeTimeout, isLoopback(bind))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	// Wait for either a fatal listen error or a shutdown signal.
	select {
	case err := <-serverErr:
		if err != nil {
			log.Fatalf("Server failed: %v", err)
		}
	case <-rootCtx.Done():
		log.Printf("Shutdown signal received; draining for up to %v", shutdownTimeout)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("Graceful shutdown failed: %v; forcing close", err)
			_ = srv.Close()
		}
		// Drain serverErr so the goroutine exits.
		<-serverErr
		log.Printf("Server stopped cleanly")
	}
}