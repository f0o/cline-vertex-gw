package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"go.f0o.dev/cline-vertex-gw/provider"
)

// buildVersion holds the binary version, populated at startup by
// SetBuildVersion(). Defaults to "dev" so tests / `go run` still get a
// reasonable value. Stored in an atomic.Value so concurrent /healthz
// requests don't race with the (single) startup write.
var buildVersion atomic.Value // string

func init() {
	buildVersion.Store("dev")
}

// SetBuildVersion is called once from main() with the -ldflags-injected
// version string before SetupRoutes. Idempotent and concurrency-safe.
func SetBuildVersion(v string) {
	if v == "" {
		v = "dev"
	}
	buildVersion.Store(v)
}

// GetBuildVersion returns the current build version. Safe for concurrent use.
func GetBuildVersion() string {
	if v, ok := buildVersion.Load().(string); ok && v != "" {
		return v
	}
	return "dev"
}

// startTime is captured at package init so /healthz can report uptime.
var startTime = time.Now()

// HealthResponse is the JSON body returned by /healthz and the legacy /.
// Kept stable so external monitors can rely on the field names.
type HealthResponse struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	UptimeSec int64  `json:"uptime_seconds"`
	GoVersion string `json:"go_version"`
}

// ReadyResponse is the JSON body returned by /readyz. The `reasons` slice is
// populated when status != "ready" so operators can tell *why* the gateway
// is not serving traffic without consulting logs.
type ReadyResponse struct {
	Status  string   `json:"status"`
	Version string   `json:"version"`
	Reasons []string `json:"reasons,omitempty"`
}

// HealthHandler is liveness — does the process respond at all? It MUST NOT
// touch upstream services or do any work that might block; otherwise a
// transient upstream outage would cause k8s/Cloud-Run to kill an otherwise
// healthy process.
//
// Also serves the legacy `/` text response for back-compat: existing probes
// that look for the "is running" substring keep working.
func (h *APIHandler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "/healthz":
		// fall through to the writer below
	default:
		http.NotFound(w, r)
		return
	}

	// Legacy `/` keeps a plain-text body so existing scrapers don't break.
	if r.URL.Path == "/" {
		fmt.Fprintf(w, "Ollama Vertex Gateway is running version=%s\n", GetBuildVersion())
		return
	}

	resp := HealthResponse{
		Status:    "ok",
		Version:   GetBuildVersion(),
		UptimeSec: int64(time.Since(startTime).Seconds()),
		GoVersion: runtime.Version(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// ReadyHandler is readiness — should this instance receive traffic? Returns
// 503 when the Vertex client wasn't configured (the operator forgot
// GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION). A cheap, allocation-free
// check that does NOT make any network calls: Vertex auth happens via
// short-lived OAuth tokens that the SDK refreshes lazily, so probing the
// upstream from /readyz would add latency without catching real outages.
//
// The intent is: "if I'm ready=true I can attempt requests; whether they
// succeed depends on the upstream which has its own retry/error model".
func (h *APIHandler) ReadyHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/readyz" {
		http.NotFound(w, r)
		return
	}
	resp := ReadyResponse{
		Status:  "ready",
		Version: GetBuildVersion(),
	}
	if h.Vertex == nil {
		resp.Status = "not_ready"
		resp.Reasons = append(resp.Reasons,
			"Vertex client not configured: set GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_LOCATION")
		writeJSON(w, http.StatusServiceUnavailable, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// VersionHandler returns just the build version as a small JSON object. Useful
// for deploy automation that wants to confirm a rollout completed without
// parsing the full /healthz body.
func (h *APIHandler) VersionHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/version" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"version": GetBuildVersion()})
}

// writeJSON is a tiny helper for the health/version handlers; the chat
// handlers do their own framing because they stream NDJSON / SSE.
func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Wire the provider-package tags-cache hit/miss callbacks into our metrics
// at package init. Done here (rather than in metrics.go) to keep the provider
// import scoped to one file and to make the dependency direction explicit:
// api depends on provider, provider does not depend on api.
func init() {
	provider.SetTagsCacheMetrics(MetricsTagsCacheHit, MetricsTagsCacheMiss)
}
