package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Metrics exposition for cline-vertex-gw. Hand-rolled Prometheus text format
// to avoid adding the ~30-transitive-dep prometheus/client_golang for what
// amounts to a handful of counters + one histogram. See exposition() for the
// full metric list and rationale.

// counterVec is a label→count map. Cardinality is bounded by call sites
// (routes static, models from a fixed publisher list, classes a small enum)
// so an unbounded-cardinality DoS is not a concern.
type counterVec struct {
	mu     sync.RWMutex
	values map[string]uint64
}

func newCounterVec() *counterVec { return &counterVec{values: make(map[string]uint64)} }

func (c *counterVec) Add(labelKey string, delta uint64) {
	c.mu.Lock()
	c.values[labelKey] += delta
	c.mu.Unlock()
}

func (c *counterVec) snapshot() []labeledValue {
	c.mu.RLock()
	out := make([]labeledValue, 0, len(c.values))
	for k, v := range c.values {
		out = append(out, labeledValue{labels: k, value: float64(v)})
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].labels < out[j].labels })
	return out
}

type labeledValue struct {
	labels string
	value  float64
}

// histogram: fixed-bucket request-duration histogram. Buckets tuned for
// typical Vertex AI latencies (TTFB 200ms-2s, full responses 1s-120s).
type histogram struct {
	mu      sync.Mutex
	buckets []float64
	counts  map[string][]uint64
	sums    map[string]float64
	totals  map[string]uint64
}

func newHistogram(buckets []float64) *histogram {
	return &histogram{
		buckets: buckets,
		counts:  make(map[string][]uint64),
		sums:    make(map[string]float64),
		totals:  make(map[string]uint64),
	}
}

func (h *histogram) Observe(labelKey string, seconds float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.counts[labelKey]; !ok {
		h.counts[labelKey] = make([]uint64, len(h.buckets))
	}
	for i, ub := range h.buckets {
		if seconds <= ub {
			h.counts[labelKey][i]++
		}
	}
	h.sums[labelKey] += seconds
	h.totals[labelKey]++
}

// metricsRegistry holds all counters.
type metricsRegistry struct {
	requests              *counterVec
	requestDuration       *histogram
	tokens                *counterVec
	retries               *counterVec
	loopDetectorFired     uint64
	tagsCacheHits         uint64
	tagsCacheMisses       uint64
	panicsRecovered       uint64
	compressionBytesSaved *counterVec
}

var metrics = &metricsRegistry{
	requests:              newCounterVec(),
	requestDuration:       newHistogram([]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}),
	tokens:                newCounterVec(),
	retries:               newCounterVec(),
	compressionBytesSaved: newCounterVec(),
}

// --- public update API ----------------------------------------------------

func MetricsRequest(route, status string, durationSec float64) {
	metrics.requests.Add(fmt.Sprintf(`route=%q,status=%q`, route, status), 1)
	metrics.requestDuration.Observe(fmt.Sprintf(`route=%q`, route), durationSec)
}

func MetricsTokens(kind, model string, n int32) {
	if n <= 0 {
		return
	}
	metrics.tokens.Add(fmt.Sprintf(`kind=%q,model=%q`, kind, model), uint64(n))
}

func MetricsRetry(class string) {
	metrics.retries.Add(fmt.Sprintf(`class=%q`, class), 1)
}

func MetricsLoopDetectorFired() { atomic.AddUint64(&metrics.loopDetectorFired, 1) }
func MetricsTagsCacheHit()      { atomic.AddUint64(&metrics.tagsCacheHits, 1) }
func MetricsTagsCacheMiss()     { atomic.AddUint64(&metrics.tagsCacheMisses, 1) }
func MetricsPanicRecovered()    { atomic.AddUint64(&metrics.panicsRecovered, 1) }

func MetricsCompressionBytesSaved(stage string, bytes int) {
	if bytes <= 0 {
		return
	}
	metrics.compressionBytesSaved.Add(fmt.Sprintf(`stage=%q`, stage), uint64(bytes))
}

// --- exposition -----------------------------------------------------------

// MetricsHandler serves Prometheus text-exposition format on /metrics.
func MetricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(exposition()))
}

// exposition builds the full Prom-format response. Split out so tests can
// assert on the body without spinning up an httptest server.
func exposition() string {
	var b strings.Builder

	writeHeader(&b, "cline_vertex_gw_build_info",
		"Build version of the cline-vertex-gw binary.", "gauge")
	fmt.Fprintf(&b, "cline_vertex_gw_build_info{version=%q} 1\n", GetBuildVersion())

	writeHeader(&b, "cline_vertex_gw_requests_total",
		"Total HTTP requests by route and final status.", "counter")
	for _, lv := range metrics.requests.snapshot() {
		fmt.Fprintf(&b, "cline_vertex_gw_requests_total{%s} %g\n", lv.labels, lv.value)
	}

	writeHeader(&b, "cline_vertex_gw_request_duration_seconds",
		"HTTP request duration by route.", "histogram")
	writeHistogramBody(&b, "cline_vertex_gw_request_duration_seconds", metrics.requestDuration)

	writeHeader(&b, "cline_vertex_gw_upstream_tokens_total",
		"Upstream token usage by kind (prompt|cached|completion) and model id.", "counter")
	for _, lv := range metrics.tokens.snapshot() {
		fmt.Fprintf(&b, "cline_vertex_gw_upstream_tokens_total{%s} %g\n", lv.labels, lv.value)
	}

	writeHeader(&b, "cline_vertex_gw_upstream_retries_total",
		"Upstream retry attempts by error class.", "counter")
	for _, lv := range metrics.retries.snapshot() {
		fmt.Fprintf(&b, "cline_vertex_gw_upstream_retries_total{%s} %g\n", lv.labels, lv.value)
	}

	writeHeader(&b, "cline_vertex_gw_upstream_loop_detector_fired_total",
		"Streams terminated by the runaway/loop detector.", "counter")
	fmt.Fprintf(&b, "cline_vertex_gw_upstream_loop_detector_fired_total %d\n",
		atomic.LoadUint64(&metrics.loopDetectorFired))

	writeHeader(&b, "cline_vertex_gw_tags_cache_hits_total",
		"/api/tags requests served from the in-memory cache.", "counter")
	fmt.Fprintf(&b, "cline_vertex_gw_tags_cache_hits_total %d\n",
		atomic.LoadUint64(&metrics.tagsCacheHits))

	writeHeader(&b, "cline_vertex_gw_tags_cache_misses_total",
		"/api/tags requests that fanned out to Vertex.", "counter")
	fmt.Fprintf(&b, "cline_vertex_gw_tags_cache_misses_total %d\n",
		atomic.LoadUint64(&metrics.tagsCacheMisses))

	writeHeader(&b, "cline_vertex_gw_panics_recovered_total",
		"Panics caught by RecoverMiddleware (>0 indicates a bug).", "counter")
	fmt.Fprintf(&b, "cline_vertex_gw_panics_recovered_total %d\n",
		atomic.LoadUint64(&metrics.panicsRecovered))

	writeHeader(&b, "cline_vertex_gw_compression_bytes_saved_total",
		"Cumulative bytes removed by each compression pipeline stage.", "counter")
	for _, lv := range metrics.compressionBytesSaved.snapshot() {
		fmt.Fprintf(&b, "cline_vertex_gw_compression_bytes_saved_total{%s} %g\n", lv.labels, lv.value)
	}

	return b.String()
}

func writeHeader(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func writeHistogramBody(b *strings.Builder, name string, h *histogram) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// stable label ordering
	labels := make([]string, 0, len(h.counts))
	for k := range h.counts {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	for _, lbl := range labels {
		var cum uint64
		for i, ub := range h.buckets {
			cum = h.counts[lbl][i]
			fmt.Fprintf(b, "%s_bucket{%s,le=\"%g\"} %d\n", name, lbl, ub, cum)
		}
		fmt.Fprintf(b, "%s_bucket{%s,le=\"+Inf\"} %d\n", name, lbl, h.totals[lbl])
		fmt.Fprintf(b, "%s_sum{%s} %g\n", name, lbl, h.sums[lbl])
		fmt.Fprintf(b, "%s_count{%s} %d\n", name, lbl, h.totals[lbl])
	}
}