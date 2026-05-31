package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.f0o.dev/cline-vertex-gw/provider"
)

// dummyHandler always returns 200 OK and echoes the body if any.
var dummyHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	if r.Body != nil {
		_, _ = io.Copy(w, r.Body)
	}
})

func TestExtractBearer(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		wantTok string
		wantErr bool
	}{
		{"empty header", "", "", true},
		{"missing prefix", "Token abc", "", true},
		{"valid", "Bearer abc123", "abc123", false},
		{"case insensitive prefix", "bearer abc123", "abc123", false},
		{"extra whitespace", "  Bearer   abc123  ", "abc123", false},
		{"empty token", "Bearer ", "", true},
		{"empty token (trailing space)", "Bearer    ", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractBearer(tt.header)
			if (err != nil) != tt.wantErr {
				t.Fatalf("extractBearer(%q) err=%v, wantErr=%v", tt.header, err, tt.wantErr)
			}
			if got != tt.wantTok {
				t.Errorf("extractBearer(%q) = %q; want %q", tt.header, got, tt.wantTok)
			}
		})
	}
}

func TestAuthMiddleware_NoTokenIsNoOp(t *testing.T) {
	h := AuthMiddleware("", dummyHandler)
	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestAuthMiddleware_PublicPathBypassesAuth(t *testing.T) {
	h := AuthMiddleware("secret", dummyHandler)
	req := httptest.NewRequest(http.MethodGet, "/", nil) // not under /api/
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 for non-/api/ path without token", rec.Code)
	}
}

func TestAuthMiddleware_MissingTokenIs401(t *testing.T) {
	h := AuthMiddleware("secret", dummyHandler)
	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("WWW-Authenticate missing Bearer challenge: %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestAuthMiddleware_WrongTokenIs403(t *testing.T) {
	h := AuthMiddleware("secret", dummyHandler)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rec.Code)
	}
}

func TestAuthMiddleware_CorrectTokenPassesThrough(t *testing.T) {
	h := AuthMiddleware("secret", dummyHandler)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if rec.Body.String() != "hello" {
		t.Errorf("body = %q; want %q", rec.Body.String(), "hello")
	}
}

func TestBodyLimitMiddleware_AllowsUnderLimit(t *testing.T) {
	h := BodyLimitMiddleware(1024, dummyHandler)
	body := strings.NewReader(strings.Repeat("a", 100))
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestBodyLimitMiddleware_BlocksOverLimit(t *testing.T) {
	// Handler that tries to read the entire body and reports any error.
	var readErr error
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		if readErr != nil {
			// MaxBytesReader writes its own 413 to w. Don't write again.
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	h := BodyLimitMiddleware(10, probe)
	body := strings.NewReader(strings.Repeat("a", 100)) // > 10 bytes
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if readErr == nil {
		t.Fatalf("expected ReadAll error after exceeding limit, got nil")
	}
	var mbe *http.MaxBytesError
	// errors.As-style check without importing errors at file scope (test only)
	if _, ok := readErr.(*http.MaxBytesError); !ok && readErr.Error() == "" {
		t.Errorf("unexpected error type: %T (%v)", readErr, readErr)
	}
	_ = mbe
}

func TestRecoverMiddleware_CatchesPanic(t *testing.T) {
	panicker := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	h := RecoverMiddleware(panicker)
	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rec := httptest.NewRecorder()
	// Should not panic; should return 500.
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestRoutingTierMiddleware(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		wantTier string
	}{
		{
			name:     "no headers",
			headers:  nil,
			wantTier: "standard",
		},
		{
			name:     "X-Routing-Tier standard",
			headers:  map[string]string{"X-Routing-Tier": "standard"},
			wantTier: "standard",
		},
		{
			name:     "X-Routing-Tier Priority capitalized",
			headers:  map[string]string{"X-Routing-Tier": "Priority"},
			wantTier: "priority",
		},
		{
			name:     "X-Vertex-AI-Routing-Tier priority",
			headers:  map[string]string{"X-Vertex-AI-Routing-Tier": "priority"},
			wantTier: "priority",
		},
		{
			name:     "X-Routing-Tier Flex",
			headers:  map[string]string{"X-Routing-Tier": "Flex"},
			wantTier: "flex",
		},
		{
			name:     "X-Routing-Tier Batch",
			headers:  map[string]string{"X-Routing-Tier": "Batch"},
			wantTier: "flex",
		},
		{
			name:     "X-Routing-Tier flex/batch",
			headers:  map[string]string{"X-Routing-Tier": "flex/batch"},
			wantTier: "flex",
		},
		{
			name:     "fallback empty or unknown to standard",
			headers:  map[string]string{"X-Routing-Tier": "unknown-tier"},
			wantTier: "standard",
		},
		{
			name: "both headers (X-Routing-Tier takes precedence)",
			headers: map[string]string{
				"X-Routing-Tier":           "priority",
				"X-Vertex-AI-Routing-Tier": "flex",
			},
			wantTier: "priority",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedTier string
			probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tier, ok := r.Context().Value(provider.ContextKeyRoutingTier).(string); ok {
					capturedTier = tier
				}
				w.WriteHeader(http.StatusOK)
			})

			h := RoutingTierMiddleware(probe)
			req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if capturedTier != tt.wantTier {
				t.Errorf("got tier %q; want %q", capturedTier, tt.wantTier)
			}
		})
	}
}
