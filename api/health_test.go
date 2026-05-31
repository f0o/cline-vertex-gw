package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.f0o.dev/cline-vertex-gw/provider"
)

func TestBuildVersionRoundTrip(t *testing.T) {
	prev := GetBuildVersion()
	t.Cleanup(func() { SetBuildVersion(prev) })

	SetBuildVersion("v9.9.9-test")
	if got := GetBuildVersion(); got != "v9.9.9-test" {
		t.Errorf("GetBuildVersion = %q, want %q", got, "v9.9.9-test")
	}

	// Empty string should fall back to "dev" so /healthz never reports "".
	SetBuildVersion("")
	if got := GetBuildVersion(); got != "dev" {
		t.Errorf("GetBuildVersion after empty Set = %q, want %q", got, "dev")
	}
}

func TestHealthHandlerLegacyRoot(t *testing.T) {
	h := &APIHandler{}
	SetBuildVersion("v1.2.3-test")
	t.Cleanup(func() { SetBuildVersion("dev") })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.HealthHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "is running") {
		t.Errorf("legacy / body missing 'is running': %q", body)
	}
	if !strings.Contains(body, "v1.2.3-test") {
		t.Errorf("legacy / body missing version: %q", body)
	}
}

func TestHealthHandlerHealthzJSON(t *testing.T) {
	h := &APIHandler{}
	SetBuildVersion("v2.0.0-test")
	t.Cleanup(func() { SetBuildVersion("dev") })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.HealthHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var resp HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %v body=%s", err, rec.Body.String())
	}
	if resp.Status != "ok" {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	if resp.Version != "v2.0.0-test" {
		t.Errorf("version = %q, want v2.0.0-test", resp.Version)
	}
	if resp.GoVersion == "" {
		t.Errorf("go_version empty")
	}
	if resp.UptimeSec < 0 {
		t.Errorf("uptime negative: %d", resp.UptimeSec)
	}
}

func TestReadyHandlerNotConfigured(t *testing.T) {
	h := &APIHandler{Vertex: nil}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ReadyHandler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var resp ReadyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %v body=%s", err, rec.Body.String())
	}
	if resp.Status != "not_ready" {
		t.Errorf("status = %q, want not_ready", resp.Status)
	}
	if len(resp.Reasons) == 0 {
		t.Errorf("reasons should be populated when not ready")
	}
}

func TestReadyHandlerConfigured(t *testing.T) {
	// We can't construct a real *VertexClient without GCP creds, but a
	// non-nil pointer is enough to exercise the "ready" branch since
	// ReadyHandler does no method calls on it.
	h := &APIHandler{Vertex: &provider.VertexClient{}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ReadyHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp ReadyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if resp.Status != "ready" {
		t.Errorf("status = %q, want ready", resp.Status)
	}
	if len(resp.Reasons) != 0 {
		t.Errorf("ready response should have no reasons: %v", resp.Reasons)
	}
}

func TestVersionHandler(t *testing.T) {
	h := &APIHandler{}
	SetBuildVersion("v3.0.0-test")
	t.Cleanup(func() { SetBuildVersion("dev") })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	h.VersionHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if resp["version"] != "v3.0.0-test" {
		t.Errorf("version = %q, want v3.0.0-test", resp["version"])
	}
}

func TestHealthRoutingMismatch(t *testing.T) {
	h := &APIHandler{}

	t.Run("Health rejects unknown path", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/anywhere", nil)
		h.HealthHandler(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("Ready rejects unknown path", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/elsewhere", nil)
		h.ReadyHandler(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("Version rejects unknown path", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v3rsion", nil)
		h.VersionHandler(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}
