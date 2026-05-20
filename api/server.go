package api

import (
	"net/http"

	"go.f0o.dev/cline-vertex-gw/provider"
)

// SetupRoutes configures the HTTP multiplexer with Ollama-compatible endpoints.
func SetupRoutes(vertexClient *provider.VertexClient) *http.ServeMux {
	mux := http.NewServeMux()
	handler := &APIHandler{Vertex: vertexClient}

	// Model discovery
	mux.HandleFunc("/api/tags", handler.TagsHandler)
	mux.HandleFunc("GET /api/tags", handler.TagsHandler)

	// Chat completions
	mux.HandleFunc("/api/chat", handler.ChatHandler)
	mux.HandleFunc("POST /api/chat", handler.ChatHandler)

	// Generate completions
	mux.HandleFunc("/api/generate", handler.GenerateHandler)
	mux.HandleFunc("POST /api/generate", handler.GenerateHandler)

	// --- OpenAI-compatible surface ---
	// These coexist with the Ollama endpoints above and reuse the same
	// internal Vertex translation primitives. Useful for clients that prefer
	// per-request `Authorization: Bearer …` headers (which Cline's Ollama
	// provider can't set) or that already speak the OpenAI Chat Completions
	// dialect (LiteLLM, LangChain, official openai SDKs, etc.).
	mux.HandleFunc("/v1/models", handler.OpenAIModelsHandler)
	mux.HandleFunc("GET /v1/models", handler.OpenAIModelsHandler)
	mux.HandleFunc("/v1/chat/completions", handler.OpenAIChatCompletionsHandler)
	mux.HandleFunc("POST /v1/chat/completions", handler.OpenAIChatCompletionsHandler)

	// Operator / probe endpoints. All unauthenticated by design so health
	// probes, deploy automation, and metrics scrapers don't need a token.
	// (Auth bypass is centralized in AuthMiddleware.isProtectedPath.)
	mux.HandleFunc("GET /healthz", handler.HealthHandler)
	mux.HandleFunc("GET /readyz", handler.ReadyHandler)
	mux.HandleFunc("GET /version", handler.VersionHandler)
	mux.HandleFunc("GET /metrics", MetricsHandler)

	// Legacy root: keep returning the old plain-text body for back-compat.
	mux.HandleFunc("/", handler.HealthHandler)

	return mux
}
