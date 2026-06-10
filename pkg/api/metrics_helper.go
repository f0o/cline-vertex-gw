package api

import (
	"context"
	"strings"
	"time"

	"go.f0o.dev/cline-vertex-gw/pkg/provider"
)

// callMetrics captures timing & token usage for a single upstream call.
//
// cachedPromptTokens is the subset of promptTokens that the upstream served
// from a prompt cache (when supported, e.g. Anthropic cache_control). It's
// surfaced separately so operators can verify GW_ANTHROPIC_PROMPT_CACHE is
// actually saving money. cachedPromptTokens <= promptTokens by construction.
type callMetrics struct {
	requestStart       time.Time
	firstChunk         time.Time
	end                time.Time
	promptTokens       int32
	cachedPromptTokens int32
	completionTokens   int32
	finishReason       string
}

// finalize returns the durations Ollama expects (in nanoseconds):
//   - total_duration   : wall clock end-to-end
//   - load_duration    : approx connection/setup latency (time-to-first-byte)
//   - prompt_eval_dur  : same as load_duration (Vertex doesn't break it out)
//   - eval_duration    : time spent streaming the completion
func (m *callMetrics) finalize() (total, load, promptDur, evalDur int64) {
	if m.end.IsZero() {
		m.end = time.Now()
	}
	total = m.end.Sub(m.requestStart).Nanoseconds()
	if !m.firstChunk.IsZero() {
		load = m.firstChunk.Sub(m.requestStart).Nanoseconds()
		promptDur = load
		evalDur = m.end.Sub(m.firstChunk).Nanoseconds()
	} else {
		// No streaming happened (or non-stream call). Spread the time evenly.
		load = total / 2
		promptDur = load
		evalDur = total - load
	}
	if evalDur < 0 {
		evalDur = 0
	}
	if promptDur < 0 {
		promptDur = 0
	}
	return
}

// doneReason normalizes Vertex finish reasons into Ollama-style strings.
// Cline reads this to detect "stop" vs other terminal conditions.
func doneReason(vertexReason string) string {
	switch strings.ToUpper(vertexReason) {
	case "", "STOP", "FINISH_REASON_STOP":
		return "stop"
	case "MAX_TOKENS", "FINISH_REASON_MAX_TOKENS":
		return "length"
	case "SAFETY", "FINISH_REASON_SAFETY":
		return "safety"
	case "RECITATION", "FINISH_REASON_RECITATION":
		return "recitation"
	default:
		return strings.ToLower(vertexReason)
	}
}

// logCompletionForModel emits a single concise summary line at request end AND
// feeds the Prometheus token / duration counters.
//
// cached_tok is the number of prompt tokens served from a prompt cache; when
// caching is configured (e.g. GW_ANTHROPIC_PROMPT_CACHE=on) it lets operators
// quickly see whether requests are hitting the cache. cached_pct is the share
// of prompt tokens that were cache-served — 0% on a cold session, ~95%+ on
// well-warmed multi-turn Cline sessions.
//
// logCompletionForModel wraps this with model labels so the per-model token
// counters get populated; callers that only know the route (not the model)
// can use logCompletion which feeds duration-only metrics.
func logCompletionForModel(ctx context.Context, rl *reqLogger, label, model string, m callMetrics) {
	logCompletion(rl, label, m)
	if model != "" {
		MetricsTokens("prompt", model, m.promptTokens-m.cachedPromptTokens)
		MetricsTokens("cached", model, m.cachedPromptTokens)
		MetricsTokens("completion", model, m.completionTokens)
		logAndMeterCost(ctx, rl, label, model, m)
	}
	total, _, _, _ := m.finalize()
	MetricsRequest(label, doneReason(m.finishReason), float64(total)/1e9)
}

// logAndMeterCost computes a per-request USD estimate from the live pricing
// table (scraped from the Cloud Billing Catalog API), prints a breakdown to
// the console alongside the request stats, and feeds the cumulative cost
// metric. When pricing for the model is unknown it prints "cost=unavailable"
// and skips the metric (no $0 noise). Cached prompt tokens are billed at the
// reduced cached-input rate; the remaining prompt tokens at the input rate.
func logAndMeterCost(ctx context.Context, rl *reqLogger, label, model string, m callMetrics) {
	bd, ok := provider.EstimateCost(ctx, model, m.promptTokens, m.cachedPromptTokens, m.completionTokens)
	if !ok {
		rl.L().Info("cost unavailable", "phase", label, "model", model, "reason", "no-pricing")
		return
	}
	rl.L().Info("cost estimate",
		"phase", label, "model", model, "approx", bd.Approx,
		"total_usd", bd.TotalUSD, "input_usd", bd.InputUSD,
		"cached_usd", bd.CachedUSD, "output_usd", bd.OutputUSD,
		"input_per_mtok", bd.InputPerM, "cached_per_mtok", bd.CachedPerM,
		"output_per_mtok", bd.OutputPerM, "src", bd.Source,
	)
	tier := "standard"
	if ctx != nil {
		if t, ok := ctx.Value(provider.ContextKeyRoutingTier).(string); ok && t != "" {
			tier = t
		}
	}
	MetricsEstimatedCost("input", model, tier, bd.InputUSD)
	MetricsEstimatedCost("cached", model, tier, bd.CachedUSD)
	MetricsEstimatedCost("output", model, tier, bd.OutputUSD)
}

// logCompletion emits a single concise summary line at request end.
func logCompletion(rl *reqLogger, label string, m callMetrics) {
	total, load, _, evalDur := m.finalize()
	var tps float64
	if evalDur > 0 && m.completionTokens > 0 {
		tps = float64(m.completionTokens) / (float64(evalDur) / 1e9)
	}
	var cachedPct float64
	if m.promptTokens > 0 {
		cachedPct = 100.0 * float64(m.cachedPromptTokens) / float64(m.promptTokens)
	}
	rl.L().Info("request done",
		"phase", label,
		"total", time.Duration(total).Truncate(time.Millisecond).String(),
		"load", time.Duration(load).Truncate(time.Millisecond).String(),
		"eval", time.Duration(evalDur).Truncate(time.Millisecond).String(),
		"prompt_tok", m.promptTokens,
		"cached_tok", m.cachedPromptTokens,
		"cached_pct", cachedPct,
		"eval_tok", m.completionTokens,
		"tps", tps,
		"reason", doneReason(m.finishReason),
	)
}
