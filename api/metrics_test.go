package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// resetMetricsForTest zeroes the global registry. Tests that mutate counters
// MUST call this in t.Cleanup or test isolation breaks.
func resetMetricsForTest(t *testing.T) {
	t.Helper()
	prev := metrics
	metrics = &metricsRegistry{
		requests:              newCounterVec(),
		requestDuration:       newHistogram([]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}),
		tokens:                newCounterVec(),
		retries:               newCounterVec(),
		compressionBytesSaved: newCounterVec(),
		estimatedCost:         newFloatCounterVec(),
	}
	t.Cleanup(func() { metrics = prev })
}

func TestMetricsEstimatedCost(t *testing.T) {
	resetMetricsForTest(t)

	MetricsEstimatedCost("input", "gemini-2.5-pro", "standard", 1.0)
	MetricsEstimatedCost("input", "gemini-2.5-pro", "standard", 0.5) // accumulates → 1.5
	MetricsEstimatedCost("cached", "gemini-2.5-pro", "priority", 0.062)
	MetricsEstimatedCost("output", "gemini-2.5-pro", "flex", 5.0)
	MetricsEstimatedCost("output", "gemini-2.5-pro", "standard", 0) // ignored (non-positive)

	body := exposition()
	mustContain(t, body, `# TYPE cline_vertex_gw_estimated_cost_usd_total counter`)
	mustContain(t, body, `cline_vertex_gw_estimated_cost_usd_total{kind="input",model="gemini-2.5-pro",tier="standard"} 1.5`)
	mustContain(t, body, `cline_vertex_gw_estimated_cost_usd_total{kind="cached",model="gemini-2.5-pro",tier="priority"} 0.062`)
	mustContain(t, body, `cline_vertex_gw_estimated_cost_usd_total{kind="output",model="gemini-2.5-pro",tier="flex"} 5`)
}

func TestMetricsHandlerEmpty(t *testing.T) {
	resetMetricsForTest(t)
	SetBuildVersion("v0.0.0-test")
	t.Cleanup(func() { SetBuildVersion("dev") })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	MetricsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain prefix", ct)
	}
	body := rec.Body.String()
	mustContain(t, body, `cline_vertex_gw_build_info{version="v0.0.0-test"} 1`)
	mustContain(t, body, `cline_vertex_gw_requests_total`)
	mustContain(t, body, `# TYPE cline_vertex_gw_request_duration_seconds histogram`)
	mustContain(t, body, `cline_vertex_gw_upstream_loop_detector_fired_total 0`)
	mustContain(t, body, `cline_vertex_gw_panics_recovered_total 0`)
}

func TestMetricsCountersUpdate(t *testing.T) {
	resetMetricsForTest(t)

	MetricsRequest("chat", "stop", 0.42)
	MetricsRequest("chat", "stop", 1.5)
	MetricsRequest("chat", "length", 5.0)
	MetricsTokens("prompt", "claude-opus-4-7", 1000)
	MetricsTokens("cached", "claude-opus-4-7", 9000)
	MetricsTokens("completion", "claude-opus-4-7", 500)
	MetricsRetry("rate-limited")
	MetricsRetry("rate-limited")
	MetricsRetry("network")
	MetricsLoopDetectorFired()
	MetricsTagsCacheHit()
	MetricsTagsCacheHit()
	MetricsTagsCacheMiss()
	MetricsPanicRecovered()
	MetricsCompressionBytesSaved("dedup", 12345)

	body := exposition()

	mustContain(t, body, `cline_vertex_gw_requests_total{route="chat",status="stop"} 2`)
	mustContain(t, body, `cline_vertex_gw_requests_total{route="chat",status="length"} 1`)
	mustContain(t, body, `cline_vertex_gw_upstream_tokens_total{kind="prompt",model="claude-opus-4-7"} 1000`)
	mustContain(t, body, `cline_vertex_gw_upstream_tokens_total{kind="cached",model="claude-opus-4-7"} 9000`)
	mustContain(t, body, `cline_vertex_gw_upstream_tokens_total{kind="completion",model="claude-opus-4-7"} 500`)
	mustContain(t, body, `cline_vertex_gw_upstream_retries_total{class="rate-limited"} 2`)
	mustContain(t, body, `cline_vertex_gw_upstream_retries_total{class="network"} 1`)
	mustContain(t, body, `cline_vertex_gw_upstream_loop_detector_fired_total 1`)
	mustContain(t, body, `cline_vertex_gw_tags_cache_hits_total 2`)
	mustContain(t, body, `cline_vertex_gw_tags_cache_misses_total 1`)
	mustContain(t, body, `cline_vertex_gw_panics_recovered_total 1`)
	mustContain(t, body, `cline_vertex_gw_compression_bytes_saved_total{stage="dedup"} 12345`)
}

func TestMetricsHistogramBuckets(t *testing.T) {
	resetMetricsForTest(t)

	// Three observations spanning multiple buckets.
	MetricsRequest("chat", "stop", 0.04) // ≤0.05
	MetricsRequest("chat", "stop", 0.40) // ≤0.5
	MetricsRequest("chat", "stop", 75.0) // ≤120
	body := exposition()

	// le="0.05" should be 1 (the 0.04 obs only).
	mustContain(t, body, `cline_vertex_gw_request_duration_seconds_bucket{route="chat",le="0.05"} 1`)
	// le="0.5" should include the 0.04 + 0.40 obs.
	mustContain(t, body, `cline_vertex_gw_request_duration_seconds_bucket{route="chat",le="0.5"} 2`)
	// le="+Inf" should be 3.
	mustContain(t, body, `cline_vertex_gw_request_duration_seconds_bucket{route="chat",le="+Inf"} 3`)
	// _count should be 3.
	mustContain(t, body, `cline_vertex_gw_request_duration_seconds_count{route="chat"} 3`)
}

func TestMetricsZeroValueIgnored(t *testing.T) {
	resetMetricsForTest(t)

	// Negative or zero token counts should not create labeled rows.
	MetricsTokens("prompt", "claude-opus-4-7", 0)
	MetricsTokens("prompt", "claude-opus-4-7", -5)
	MetricsCompressionBytesSaved("normalize", 0)

	body := exposition()
	if strings.Contains(body, `cline_vertex_gw_upstream_tokens_total{kind="prompt",model="claude-opus-4-7"}`) {
		t.Errorf("zero/negative token count should not produce a counter row, got:\n%s", body)
	}
	if strings.Contains(body, `cline_vertex_gw_compression_bytes_saved_total{stage="normalize"}`) {
		t.Errorf("zero compression saving should not produce a counter row")
	}
}

func TestMetricsConcurrentSafety(t *testing.T) {
	resetMetricsForTest(t)

	var wg sync.WaitGroup
	const goroutines = 50
	const perGoroutine = 100
	var added int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				MetricsRequest("chat", "stop", 0.1)
				MetricsTokens("completion", "test-model", 1)
				MetricsLoopDetectorFired()
				atomic.AddInt64(&added, 1)
			}
		}()
	}
	wg.Wait()

	want := uint64(goroutines * perGoroutine)
	if got := atomic.LoadUint64(&metrics.loopDetectorFired); got != want {
		t.Errorf("loop_detector counter = %d, want %d", got, want)
	}
	body := exposition()
	mustContain(t, body, `cline_vertex_gw_upstream_tokens_total{kind="completion",model="test-model"} 5000`)
}

func mustContain(t *testing.T, body, needle string) {
	t.Helper()
	if !strings.Contains(body, needle) {
		t.Errorf("metrics body missing expected line %q\n--- body ---\n%s", needle, body)
	}
}
