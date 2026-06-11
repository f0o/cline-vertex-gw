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

	"go.f0o.dev/cline-vertex-gw/pkg/api"
	"go.f0o.dev/cline-vertex-gw/pkg/cache"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"go.f0o.dev/cline-vertex-gw/pkg/pipeline"
	"go.f0o.dev/cline-vertex-gw/pkg/provider"
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
	envLogFormat             = "LOG_FORMAT" // "json" (default) or "text"
	envLogLevel              = "LOG_LEVEL"  // "debug"|"info"|"warn"|"error"; default "info"
	envElidedCleanupInterval = "GW_ELIDED_CLEANUP_INTERVAL_SEC" // optional; default 600 (10 minutes). 0 disables.
	envElidedTTL             = "GW_ELIDED_TTL_SEC"              // optional; default 10800 (3 hours).
)

// configureLogging installs a slog handler as the program-wide default and
// also reroutes the legacy `log` package through it via a severity-aware
// bridge. Application code logs through the leveled logx helpers (which carry
// the correct level + structured attributes); the bridge is a safety net so
// stdlib/third-party `log` output still respects LOG_LEVEL.
//
// LOG_LEVEL selects the minimum level emitted ("debug"|"info"|"warn"|"error",
// default "info"). LOG_FORMAT=text falls back to the human-readable text
// handler for local development; default is JSON for production /
// aggregator-friendly use (one structured record per event).

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
	// Bridge stdlib log → slog so any un-migrated / third-party log.Printf
	// output still becomes structured and severity-classified (see slogBridge).
	log.SetFlags(0)

	log.SetOutput(&slogBridge{logger: logger})
}

// slogBridge wraps a slog.Logger as an io.Writer so any *un-migrated*
// log.Printf output still flows through the structured handler. Most call sites
// have been moved to the leveled logx helpers; this bridge remains as a safety
// net for the stdlib `log` package and third-party libraries that log through
// it.
//
// Crucially, the bridge is severity-aware: instead of forcing every line to
// INFO (which made LOG_LEVEL useless), it infers the level from conventional
// markers in the message text. This guarantees that even legacy/third-party
// lines respect LOG_LEVEL filtering. Migrated call sites set the level
// explicitly via logx and never rely on this inference.
type slogBridge struct{ logger *slog.Logger }

func (b *slogBridge) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	b.logger.Log(context.Background(), inferLevel(msg), msg)
	return len(p), nil
}

// inferLevel maps a freeform log line to a slog level using common severity
// markers. It is intentionally conservative: anything unrecognized stays at
// INFO so we never accidentally hide a normal lifecycle line. Order matters —
// more severe markers are checked first.
func inferLevel(msg string) slog.Level {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "[debug]"):
		return slog.LevelDebug
	case strings.Contains(msg, "SECURITY WARNING:"),
		strings.Contains(msg, "WARNING:"),
		strings.Contains(lower, "invalid "),
		strings.Contains(lower, "using stale"),
		strings.Contains(lower, "using default"):
		return slog.LevelWarn
	case strings.Contains(lower, "panic"),
		strings.Contains(lower, "failed"),
		strings.Contains(lower, "error:"):
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func mustEnvInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		logx.Warn("invalid env value; using default",
			"env", name, "value", v, "default", def)
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

	// Handle MCP subcommand
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		api.RunMCPServer()
		return
	}

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
		logx.Warn("Vertex AI project/location not set; upstream calls will fail until configured",
			"project_env", envProject, "location_env", envLocation)
	}

	authToken := strings.TrimSpace(os.Getenv(envAuthToken))
	if authToken == "" {
		logx.Warn("gateway running UNAUTHENTICATED; set the auth token to require Bearer auth on /api/* and /v1/*",
			"auth_env", envAuthToken)
	}
	// Loud warning if the operator opted to bind non-loopback without auth —
	// this is the single most dangerous misconfiguration possible, so it is an
	// ERROR (still non-fatal: the operator may front this with a proxy).
	if authToken == "" && !isLoopback(bind) {
		logx.Error("SECURITY: bound to a non-loopback address WITHOUT auth — anyone reachable can spend your Vertex AI quota",
			"bind", bind, "auth_env", envAuthToken, "bind_env", envBind)
	}

	maxBodyMB := mustEnvInt(envMaxBodyMB, 16)
	readHeaderTimeout := time.Duration(mustEnvInt(envReadHeader, 10)) * time.Second
	idleTimeout := time.Duration(mustEnvInt(envIdle, 120)) * time.Second
	// WriteTimeout MUST default to 0 (no limit): completions stream for many
	// seconds; a fixed write deadline would truncate them. Operators can opt in
	// if they front this with a load-balancer that enforces its own timeout.
	writeTimeout := time.Duration(mustEnvInt(envWrite, 0)) * time.Second
	shutdownTimeout := time.Duration(mustEnvInt(envShutdown, 30)) * time.Second

	elidedCleanupInterval := time.Duration(mustEnvInt(envElidedCleanupInterval, 600)) * time.Second
	elidedTTL := time.Duration(mustEnvInt(envElidedTTL, 10800)) * time.Second

	// Root context cancelled on SIGINT/SIGTERM so in-flight upstream calls can
	// exit promptly via context propagation.
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var vertexClient *provider.VertexClient
	if projectID != "" && location != "" {
		vc, err := provider.NewVertexClient(rootCtx, projectID, location)
		if err != nil {
			logx.Fatal("failed to initialize Vertex AI client", "error", err)
		}

		vertexClient = vc
		defer vertexClient.Close()

		// Warm the live pricing table (scraped from the Cloud Billing Catalog
		// API) so the first request can already show a cost estimate. Runs in
		// a goroutine — best-effort, never blocks startup, and a billing-API
		// hiccup just means estimates appear after the first background
		// refresh. Disabled when GW_PRICING=off.
		if provider.PricingEnabled() {
			go vertexClient.WarmPricing(rootCtx)
		}

		// Warm the model catalog from the on-disk filesystem cache (lazy
		// refresh: loads from disk when fresh, otherwise runs the live
		// multi-publisher discovery and rewrites the cache). Runs in a
		// goroutine — best-effort, never blocks startup; the model picker
		// falls back to live discovery on the first /api/tags poll if this
		// hasn't completed.
		go vertexClient.WarmModels(rootCtx)
	}

	// Start the automated/periodic cleanup of elided files if enabled.
	if elidedCleanupInterval > 0 {
		go func() {
			// Run an initial cleanup shortly after startup (e.g., 5 seconds) to avoid blocking startup
			// but still clean up any stale files from previous runs.
			select {
			case <-rootCtx.Done():
				return
			case <-time.After(5 * time.Second):
				deleted, err := cache.CleanupElidedFiles(elidedTTL)
				if err != nil {
					logx.Error("failed to run initial elided files cleanup", "error", err)
				} else if deleted > 0 {
					logx.Info("initial cleanup deleted stale elided files", "count", deleted)
				}
			}

			ticker := time.NewTicker(elidedCleanupInterval)
			defer ticker.Stop()
			for {
				select {
				case <-rootCtx.Done():
					return
				case <-ticker.C:
					deleted, err := cache.CleanupElidedFiles(elidedTTL)
					if err != nil {
						logx.Error("failed to cleanup elided files", "error", err)
					} else if deleted > 0 {
						logx.Info("automated cleanup deleted stale elided files", "count", deleted)
					}
				}
			}
		}()
	}

	// Publish the build version so /healthz and structured-log preambles can
	// surface it. Done before SetupRoutes so the health handler sees it.
	api.SetBuildVersion(version)

	mux := api.SetupRoutes(vertexClient)

	// Wrap handlers with: recover-from-panic -> routing-tier -> auth -> body-size-limit.
	// Order matters: panic recovery is the outermost layer so any later
	// middleware that panics still returns a clean 500. Auth runs before the
	// body limiter so unauthorized clients can't consume the body budget.
	handler := api.RecoverMiddleware(
		api.RoutingTierMiddleware(
			api.AuthMiddleware(authToken,
				api.BodyLimitMiddleware(int64(maxBodyMB)*1024*1024, mux))))

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
		pipeline.LogOptimizerPipelineConfiguration()

		logx.Info("starting Cline Vertex Gateway",
			"version", version, "addr", addr, "auth", authToken != "",
			"max_body_mb", maxBodyMB, "write_timeout", writeTimeout.String(),
			"loopback", isLoopback(bind))

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
			logx.Fatal("server failed", "error", err)
		}
	case <-rootCtx.Done():
		logx.Info("shutdown signal received; draining", "timeout", shutdownTimeout.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logx.Error("graceful shutdown failed; forcing close", "error", err)
			_ = srv.Close()
		}
		// Drain serverErr so the goroutine exits.
		<-serverErr
		logx.Info("server stopped cleanly")
	}

}
