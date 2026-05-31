package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"runtime/debug"
	"strings"

	"go.f0o.dev/cline-vertex-gw/logx"
	"go.f0o.dev/cline-vertex-gw/provider"
)

// RecoverMiddleware catches any panic in downstream handlers, logs the stack,
// and returns a 500 to the client. Without this a panicking handler would
// kill the entire server process.
func RecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				MetricsPanicRecovered()
				logx.For("http").Error("panic recovered while handling request",
					"method", r.Method, "path", r.URL.Path,
					"panic", rec, "stack", string(debug.Stack()))

				// If headers haven't been flushed yet we can still write a
				// proper 500 — if they have (streaming), the connection is
				// effectively dead; just abort.
				w.Header().Set("Content-Type", "application/json")
				http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// BodyLimitMiddleware caps incoming request bodies at maxBytes. Reading past
// the limit yields an error in the handler, which is then surfaced as 413.
//
// Health-check / root requests are passed through unmodified since they don't
// have bodies worth limiting and we want zero overhead there.
func BodyLimitMiddleware(maxBytes int64, next http.Handler) http.Handler {
	if maxBytes <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.ContentLength != 0 {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// AuthMiddleware optionally enforces a shared bearer token on every /api/*
// and /v1/* request. If expected is empty, the middleware is a no-op (matches
// prior permissive behavior, with a startup warning emitted in main).
//
// Public endpoints outside the protected prefixes (currently just `/`) remain
// unauthenticated so health probes / load balancers don't need credentials.
//
// Comparison is constant-time to avoid timing oracles.
func AuthMiddleware(expected string, next http.Handler) http.Handler {
	if expected == "" {
		return next
	}
	expectedBytes := []byte(expected)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isProtectedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		got, err := extractBearer(r.Header.Get("Authorization"))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cline-vertex-gw"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), expectedBytes) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cline-vertex-gw", error="invalid_token"`)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isProtectedPath reports whether r.URL.Path falls under one of the API
// prefixes that require bearer-token authentication when one is configured.
// Currently `/api/*` (Ollama-shaped) and `/v1/*` (OpenAI-shaped).
func isProtectedPath(path string) bool {
	return strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/v1/")
}

// extractBearer parses an `Authorization: Bearer <token>` header.
// Returns an error for missing / malformed values. Empty header => unauthenticated.
func extractBearer(h string) (string, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", errors.New("missing Authorization header")
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", errors.New("malformed Authorization header")
	}

	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", errors.New("empty bearer token")
	}
	return token, nil
}

// RoutingTierMiddleware extracts the routing tier from the incoming request headers,
// normalizes it, and injects it into the request context.
func RoutingTierMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tier := r.Header.Get("X-Routing-Tier")
		if tier == "" {
			tier = r.Header.Get("X-Vertex-AI-Routing-Tier")
		}

		normalized := "standard"
		switch strings.ToLower(strings.TrimSpace(tier)) {
		case "priority":
			normalized = "priority"
		case "flex", "batch", "flex/batch":
			normalized = "flex"
		default:
			normalized = "standard"
		}

		ctx := context.WithValue(r.Context(), provider.ContextKeyRoutingTier, normalized)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
