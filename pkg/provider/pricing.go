package provider

import (
	"context"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// logPricingScrape scopes billing-catalog scrape logs to component=pricing.
var logPricingScrape = logx.Scoped("pricing")

// pricingDebug reports whether verbose pricing diagnostics should be emitted.
// Enabled by GW_PRICING_DEBUG=on/true/1 OR by running at LOG_LEVEL=debug.
func pricingDebug() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GW_PRICING_DEBUG"))) {
	case "on", "true", "1", "yes":
		return true
	}
	return false
}

// tokenKind enumerates the billable token categories we recognize. Cached
// input tokens are billed at a reduced rate by every publisher that supports
// prompt caching, so we track them separately from regular input tokens.
type tokenKind int

const (
	kindInput tokenKind = iota
	kindCachedInput
	kindOutput
	kindUnknown
)

// ModelPrice holds resolved per-1M-token USD rates for a single model.
//
// A zero rate means "not found in the catalog" for that kind; callers must
// check Known() / the per-field presence before trusting a value. CachedPerM
// falls back to InputPerM at estimate time when the catalog lists no dedicated
// cached-input SKU (some publishers fold cached input into the input SKU).
type ModelPrice struct {
	InputPerM  float64 // USD per 1M input (prompt) tokens
	CachedPerM float64 // USD per 1M cached-input tokens (0 = no dedicated SKU)
	OutputPerM float64 // USD per 1M output (completion) tokens
	Source     string  // billing service displayName the SKUs came from
	Approx     bool    // true when derived from character-priced SKUs
}

// Known reports whether we resolved at least the input and output rates, which
// is the minimum needed to produce a meaningful estimate.
func (p ModelPrice) Known() bool { return p.InputPerM > 0 || p.OutputPerM > 0 }

// CostBreakdown is the per-request cost estimate returned by EstimateCost.
// All monetary fields are in USD.
type CostBreakdown struct {
	InputUSD  float64
	CachedUSD float64
	OutputUSD float64
	TotalUSD  float64

	// Rates actually used (USD per 1M tokens), surfaced so the console line
	// can show operators which numbers produced the estimate.
	InputPerM  float64
	CachedPerM float64
	OutputPerM float64

	Source string
	Approx bool
}

// avgCharsPerToken is a rough conversion used only for character-priced SKUs
// (a small minority — most Gen-AI SKUs are already token-priced). Estimates
// derived this way are flagged Approx=true.
const avgCharsPerToken = 4.0

// modelFamilyRe is the final, anchored family matcher applied to the cleaned
// string. It captures the family + version/variant run.
var modelFamilyRe = regexp.MustCompile(`(?i)^(` +
	`gemini[\w.\- ]*` +
	`|claude[\w.\- ]*` +
	`|llama[\w.\- ]*` +
	`|codestral[\w.\- ]*` +
	`|mixtral[\w.\- ]*` +
	`|mistral[\w.\- ]*` +
	`|jamba[\w.\- ]*` +
	`|command[\w.\- ]*` +
	`|deepseek[\w.\- ]*` +
	`|qwen[\w.\- ]*` +
	`|gemma[\w.\- ]*` +
	`)`)

// --- pricing table (process-global, populated by RefreshPricing) -----------

type pricingState struct {
	mu    sync.RWMutex
	table map[string]ModelPrice // keyed by normalized model id
	at    time.Time
}

var pricing = &pricingState{table: map[string]ModelPrice{}}

// setTable atomically replaces the pricing table.
func (s *pricingState) setTable(t map[string]ModelPrice) {
	s.mu.Lock()
	s.table = t
	s.at = time.Now()
	s.mu.Unlock()
}

// snapshot returns a shallow copy of the current pricing table for
// serialization to the on-disk cache. Returns nil when the table is empty so
// callers can skip persisting a useless empty file.
func (s *pricingState) snapshot() map[string]ModelPrice {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.table) == 0 {
		return nil
	}
	out := make(map[string]ModelPrice, len(s.table))
	for k, v := range s.table {
		out[k] = v
	}
	return out
}

// versionAgnosticKey strips all numeric digit characters from a normalized key.
// This allows matching across version numbers, e.g. "claudeopus48" and "claude3opus"
// both map to "claudeopus", and "gemini35flash" and "gemini25flash" map to "geminiflash".
func versionAgnosticKey(k string) string {
	var sb strings.Builder
	for _, ch := range k {
		if ch < '0' || ch > '9' {
			sb.WriteRune(ch)
		}
	}
	return sb.String()
}

// lookup returns the best ModelPrice for a model id using longest-prefix
// matching against the normalized table keys (so "gemini-2.5-pro-preview-..."
// still resolves to the "gemini-2.5-pro" entry).
func (s *pricingState) lookup(model string) (ModelPrice, bool) {
	_, modelID := ParsePublisher(model)
	key := normalizePriceKey(modelID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if mp, ok := s.table[key]; ok {
		return mp, true
	}
	// longest-match fallback: pick the table key that is the longest
	// substring/prefix relationship with the requested key.
	var best string
	for k := range s.table {
		if k == "" {
			continue
		}
		if strings.HasPrefix(key, k) || strings.HasPrefix(k, key) {
			if len(k) > len(best) {
				best = k
			}
		}
	}
	if best != "" {
		return s.table[best], true
	}

	// version-agnostic fallback: strip digits to match overall family, e.g.
	// resolving unreleased requested model "gemini-3.5-flash" (normalized "gemini35flash" -> "geminiflash")
	// to "gemini25flash" (normalized "gemini25flash" -> "geminiflash").
	targetAgnostic := versionAgnosticKey(key)
	var bestAgnostic string
	for k := range s.table {
		if k == "" {
			continue
		}
		if versionAgnosticKey(k) == targetAgnostic {
			// Select lexicographically larger/longer key to stabilize on latest version
			if bestAgnostic == "" || k > bestAgnostic {
				bestAgnostic = k
			}
		}
	}
	if bestAgnostic != "" {
		return s.table[bestAgnostic], true
	}

	return ModelPrice{}, false
}

// LookupPrice returns the resolved price for a model, if known.
func LookupPrice(model string) (ModelPrice, bool) { return pricing.lookup(model) }

// EstimateCost computes a per-request cost estimate from the cached pricing
// table. promptTokens is the TOTAL prompt tokens (including cached);
// cachedTokens is the subset served from a prompt cache. The non-cached prompt
// tokens are billed at the input rate and the cached subset at the cached rate
// (falling back to the input rate when no dedicated cached SKU exists).
//
// ok is false when pricing for the model is unknown — callers should then
// print "cost unavailable" and skip cost metrics rather than report $0.
func EstimateCost(ctx context.Context, model string, promptTokens, cachedTokens, completionTokens int32) (CostBreakdown, bool) {
	if !PricingEnabled() {
		return CostBreakdown{}, false
	}

	tier := "standard"
	if ctx != nil {
		if t, ok := ctx.Value(ContextKeyRoutingTier).(string); ok && t != "" {
			tier = t
		}
	}

	// Try the tier-specific entry first; fall back to the standard rate when a
	// model doesn't offer that tier (not every model has Priority/Flex). The
	// suffix matches the keys produced by the HTML scraper and resolveSKU.
	var mp ModelPrice
	var found bool
	if sfx := tierSuffix(tier); sfx != "" {
		mp, found = pricing.lookup(model + sfx)
	}
	if !found {
		mp, found = pricing.lookup(model)
	}

	if !found || !mp.Known() {
		return CostBreakdown{}, false
	}

	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > promptTokens {
		cachedTokens = promptTokens
	}
	nonCached := promptTokens - cachedTokens

	cachedRate := mp.CachedPerM
	if cachedRate <= 0 {
		// No dedicated cached SKU: cached tokens are billed as normal input.
		cachedRate = mp.InputPerM
	}

	const perM = 1_000_000.0
	bd := CostBreakdown{
		InputUSD:   float64(nonCached) / perM * mp.InputPerM,
		CachedUSD:  float64(cachedTokens) / perM * cachedRate,
		OutputUSD:  float64(completionTokens) / perM * mp.OutputPerM,
		InputPerM:  mp.InputPerM,
		CachedPerM: cachedRate,
		OutputPerM: mp.OutputPerM,
		Source:     mp.Source,
		Approx:     mp.Approx,
	}
	bd.TotalUSD = bd.InputUSD + bd.CachedUSD + bd.OutputUSD
	return bd, true
}

const billingAPIBase = "https://cloudbilling.googleapis.com/v1"
