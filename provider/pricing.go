package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"go.f0o.dev/cline-vertex-gw/logx"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// logPricingScrape scopes billing-catalog scrape logs to component=pricing.
var logPricingScrape = logx.Scoped("pricing")

// Live model-pricing discovery via the Cloud Billing Catalog API.
//
// Rationale: hardcoding per-model token prices is a maintenance burden and
// drifts out of date the moment Google adjusts pricing. Instead — exactly like
// ListModels scrapes the publisher catalogs — we scrape the authoritative
// pricing source at runtime:
//
//	GET https://cloudbilling.googleapis.com/v1/services?pageSize=...
//	GET https://cloudbilling.googleapis.com/v1/services/{id}/skus?currencyCode=USD
//
// SKU descriptions are free-text (e.g. "Gemini 2.5 Pro Input Token Count")
// so a deterministic resolver maps each SKU to a (model, token-kind) pair and
// normalizes every rate to USD per 1,000,000 tokens. Anything we can't
// confidently resolve is simply omitted — a model with unknown pricing reports
// "cost unavailable" rather than a wrong number (correctness by omission).
//
// All failures are best-effort and never block a chat request: the estimate is
// computed from a cached table, and a missing/failed table just means no cost
// line is printed.

const billingAPIBase = "https://cloudbilling.googleapis.com/v1"

// pricingDebug reports whether verbose pricing diagnostics should be emitted.
// Enabled by GW_PRICING_DEBUG=on/true/1 OR by running at LOG_LEVEL=debug.
// When on, RefreshPricing dumps the matched/non-matched billing services,
// a per-SKU resolution trace, and the final per-model resolved rates so the
// SKU→model/rate mapping can be verified against real catalog data.
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

// --- scraping --------------------------------------------------------------

// RefreshPricing scrapes the Cloud Billing Catalog and rebuilds the global
// pricing table. It is best-effort: on any error the previous table is kept
// and the error is returned for logging. Safe to call concurrently; the table
// swap is atomic.
//
// We scan EVERY billing service rather than filtering by service name. The
// service name is an unreliable signal — Google files the "AI Dev Tools:
// Claude …" token SKUs under the **"Vertex AI Search"** service, Gemini/MaaS
// under "Vertex AI", and may refile models again at any time. Instead the
// strict per-SKU resolver (`resolveSKU`) is the gate: it only accepts a SKU
// that is token/character-priced, isn't Batch/cache-write, and whose
// description matches a known model family. Non-LLM services contribute no
// resolvable SKUs and are effectively ignored for free.
func (vc *VertexClient) RefreshPricing(ctx context.Context) error {
	start := time.Now()
	dbg := pricingDebug()

	table := map[string]ModelPrice{}

	// 1. Scrape the HTML pricing page (ModelCard/Docs ground truth)
	if err := vc.scrapeHTMLPricing(ctx, table); err != nil {
		logPricingScrape.Warnf("HTML docs scraping failed: %v", err)
	} else {
		logPricingScrape.Infof("HTML docs scraping completed, resolved %d model rates", len(table))
	}

	// 2. Scrape the Cloud Billing API (merged on top of HTML rates)
	services, err := vc.listBillingServices(ctx)
	if err != nil {
		return fmt.Errorf("list billing services: %w", err)
	}

	skuCount := 0
	servicesWithPrices := 0
	for _, svc := range services {
		// Optimization: Filter out non-AI billing services immediately
		// to prevent background worker timeouts and API quota exhaustion.
		dn := strings.ToLower(svc.DisplayName)
		if !strings.Contains(dn, "vertex") && !strings.Contains(dn, "ai") &&
			!strings.Contains(dn, "anthropic") && !strings.Contains(dn, "cohere") {
			continue
		}

		skus, serr := vc.listSKUs(ctx, svc.Name)
		if serr != nil {
			// Most services 403/404 for a given project; that's expected when
			// scanning the whole catalog. Only log at debug to avoid noise.
			if dbg {
				logPricingScrape.Debugf("service=%q skus failed: %v", svc.DisplayName, serr)
			}
			continue
		}
		skuCount += len(skus)
		before := len(table)
		mergeSKUsIntoTable(table, skus, svc.DisplayName, dbg)
		if len(table) > before {
			servicesWithPrices++
			if dbg {
				logPricingScrape.Debugf("service display=%q id=%q skus=%d contributed=%d",
					svc.DisplayName, svc.Name, len(skus), len(table)-before)
			}
		}
	}

	if dbg {
		dumpPricingTable(table)
	}

	if len(table) == 0 {
		return fmt.Errorf("scanned %d services, %d skus, but resolved 0 model prices", len(services), skuCount)
	}

	pricing.setTable(table)
	logPricingScrape.Infof("services_scanned=%d services_with_prices=%d skus_seen=%d models_priced=%d elapsed=%v",
		len(services), servicesWithPrices, skuCount, len(table), time.Since(start))
	return nil
}

// dumpPricingTable logs every resolved model + its rates, sorted by key, so the
// SKU→model/rate mapping can be eyeballed against real catalog data. Only
// called when pricingDebug() is on.
func dumpPricingTable(table map[string]ModelPrice) {
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		mp := table[k]
		logPricingScrape.Debugf("model=%q in=$%.4f cached=$%.4f out=$%.4f approx=%v src=%q",
			k, mp.InputPerM, mp.CachedPerM, mp.OutputPerM, mp.Approx, mp.Source)
	}
}

// billingService is the subset of a Cloud Billing service we care about.
type billingService struct {
	Name        string `json:"name"`        // "services/XXXX-XXXX-XXXX"
	DisplayName string `json:"displayName"` // "Vertex AI"
}

// fetchPaged walks a Cloud Billing list endpoint that uses the standard
// `nextPageToken` pagination convention and accumulates every item. buildURL
// receives the current page token ("" for the first page) and returns the full
// request URL; extract pulls the items and next-page token out of a decoded
// page. The page type P must be a struct with a NextPageToken field exposed via
// extract — kept generic so both the services and SKUs lists share one loop.
func fetchPaged[T any, P any](
	ctx context.Context,
	vc *VertexClient,
	buildURL func(pageToken string) string,
	extract func(page *P) (items []T, nextPageToken string),
) ([]T, error) {
	var out []T
	pageToken := ""
	for {
		var page P
		if err := vc.getBillingJSON(ctx, buildURL(pageToken), &page); err != nil {
			return nil, err
		}
		items, next := extract(&page)
		out = append(out, items...)
		if next == "" {
			return out, nil
		}
		pageToken = next
	}
}

func (vc *VertexClient) listBillingServices(ctx context.Context) ([]billingService, error) {
	type page struct {
		Services      []billingService `json:"services"`
		NextPageToken string           `json:"nextPageToken"`
	}
	return fetchPaged(ctx, vc,
		func(pageToken string) string {
			url := fmt.Sprintf("%s/services?pageSize=200", billingAPIBase)
			if pageToken != "" {
				url += "&pageToken=" + pageToken
			}
			return url
		},
		func(p *page) ([]billingService, string) { return p.Services, p.NextPageToken },
	)
}

// rawSKU is the subset of a Cloud Billing SKU we parse. Rates live under
// pricingInfo[].pricingExpression.tieredRates[].unitPrice.
type rawSKU struct {
	Description string `json:"description"`
	Category    struct {
		ResourceFamily string `json:"resourceFamily"`
		ResourceGroup  string `json:"resourceGroup"`
		UsageType      string `json:"usageType"`
	} `json:"category"`
	PricingInfo []struct {
		PricingExpression struct {
			UsageUnit            string `json:"usageUnit"`            // e.g. "1M tokens", "1k characters"
			UsageUnitDescription string `json:"usageUnitDescription"` // e.g. "1 million tokens"
			TieredRates          []struct {
				UnitPrice struct {
					CurrencyCode string `json:"currencyCode"`
					Units        string `json:"units"` // whole units, as string
					Nanos        int64  `json:"nanos"` // billionths
				} `json:"unitPrice"`
			} `json:"tieredRates"`
		} `json:"pricingExpression"`
	} `json:"pricingInfo"`
}

func (vc *VertexClient) listSKUs(ctx context.Context, serviceName string) ([]rawSKU, error) {
	type page struct {
		SKUs          []rawSKU `json:"skus"`
		NextPageToken string   `json:"nextPageToken"`
	}
	return fetchPaged(ctx, vc,
		func(pageToken string) string {
			url := fmt.Sprintf("%s/%s/skus?currencyCode=USD&pageSize=5000", billingAPIBase, serviceName)
			if pageToken != "" {
				url += "&pageToken=" + pageToken
			}
			return url
		},
		func(p *page) ([]rawSKU, string) { return p.SKUs, p.NextPageToken },
	)
}

// getBillingJSON performs an authenticated GET against the Cloud Billing API
// and decodes the JSON body into v. Uses the short-timeout discovery client.
func (vc *VertexClient) getBillingJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if vc.projectID != "" {
		req.Header.Set("x-goog-user-project", vc.projectID)
	}
	resp, err := vc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// --- resolver --------------------------------------------------------------

// mergeSKUsIntoTable resolves each SKU to (model, kind, ratePerM) and folds it
// into the table, keeping the most specific (longest description) match per
// (model, kind). source is the billing service displayName.
func mergeSKUsIntoTable(table map[string]ModelPrice, skus []rawSKU, source string, dbg bool) {
	for _, sku := range skus {
		model, kind, approx := resolveSKU(sku)
		if model == "" || kind == kindUnknown {
			continue
		}
		ratePerM, ok := skuRatePerMillionTokens(sku)
		if dbg {
			unit := ""
			units := ""
			var nanos int64
			ntiers := 0
			if len(sku.PricingInfo) > 0 {
				pe := sku.PricingInfo[0].PricingExpression
				unit = pe.UsageUnit
				ntiers = len(pe.TieredRates)
				if ntiers > 0 {
					units = pe.TieredRates[0].UnitPrice.Units
					nanos = pe.TieredRates[0].UnitPrice.Nanos
				}
			}
			logPricingScrape.Debugf("sku desc=%q unit=%q tiers=%d tier0(units=%q nanos=%d) -> model=%q kind=%s ratePerM=$%.4f ok=%v",
				sku.Description, unit, ntiers, units, nanos, model, kindName(kind), ratePerM, ok)
		}
		if !ok || ratePerM <= 0 {
			continue
		}

		mp := table[model]
		fromHTML := mp.Source == "HTML Scraper"
		mp.Source = source
		if approx {
			mp.Approx = true
		}
		switch kind {
		case kindInput:
			// Prefer the lowest input rate (base price, not the long-context
			// premium tier) for a representative estimate.
			if fromHTML || mp.InputPerM == 0 || ratePerM < mp.InputPerM {
				mp.InputPerM = ratePerM
			}

		case kindCachedInput:
			if fromHTML || mp.CachedPerM == 0 || ratePerM < mp.CachedPerM {
				mp.CachedPerM = ratePerM
			}
		case kindOutput:
			if fromHTML || mp.OutputPerM == 0 || ratePerM < mp.OutputPerM {
				mp.OutputPerM = ratePerM
			}
		}
		table[model] = mp
	}
}

// kindName returns a short label for a tokenKind (used in debug logs).
func kindName(k tokenKind) string {
	switch k {
	case kindInput:
		return "input"
	case kindCachedInput:
		return "cached"
	case kindOutput:
		return "output"
	default:
		return "unknown"
	}
}

// kindFromDescription classifies a SKU description into a token kind. Order
// matters: "cache" must be checked before generic "input"/"prompt" since a
// cached-input SKU contains both words.
func kindFromDescription(desc string) tokenKind {
	d := strings.ToLower(desc)
	switch {
	case strings.Contains(d, "cache"):
		// Context/prompt caching SKUs are reduced-rate input. (Cache write
		// SKUs are also classed here — see noteWriteVsRead handling below.)
		return kindCachedInput
	case strings.Contains(d, "output") || strings.Contains(d, "completion") ||
		strings.Contains(d, "response"):
		return kindOutput
	case strings.Contains(d, "input") || strings.Contains(d, "prompt"):
		return kindInput
	}
	return kindUnknown
}

// modelNoiseRe strips the boilerplate that wraps a model name in real SKU
// descriptions so only the model identity remains. Applied (case-insensitively)
// before the family regex. Real examples this cleans:
//
//	"AI Dev Tools: Claude Opus 4.6 Input Tokens (Long)" -> "claude opus 4.6"
//	"Cloud Vertex AI Model Garden Model as a Service Llama 4 Scout Output Tokens" -> "llama 4 scout"
//	"Gemini 2.5 Pro Input Token Count (Prompts <= 200K)" -> "gemini 2.5 pro"
var (
	modelNoisePrefixRe = regexp.MustCompile(`(?i)^(ai dev tools:|cloud vertex ai model garden model as a service|cloud vertex ai|vertex ai|model garden|model as a service)\s*`)
	// Trailing kind/qualifier words + everything after the first of them.
	modelNoiseTailRe = regexp.MustCompile(`(?i)\s+(input|output|prompt|completion|response|cache|cached|context|token|tokens|count|batch|flex|priority|long|standard|write|read)\b.*$`)
	parensRe         = regexp.MustCompile(`\s*\(.*?\)\s*`)
)

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

// cleanModelName strips wrapping boilerplate, parenthetical qualifiers, and the
// trailing token-kind words from a SKU description, leaving just the model
// identity (e.g. "claude opus 4.6", "llama 4 scout", "gemini 2.5 pro").
func cleanModelName(desc string) string {
	s := strings.TrimSpace(desc)
	s = parensRe.ReplaceAllString(s, " ")
	s = modelNoisePrefixRe.ReplaceAllString(s, "")
	s = modelNoiseTailRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// resolveSKU returns the normalized model key and token kind for a SKU, plus
// whether the price is character-based (approximate). Returns "" model when it
// can't confidently identify a Gen-AI token SKU.
//
// "Batch" SKUs are resolved with a :flex suffix so that the dynamic cost
// estimator can look up the correct Batch/Flex prices on demand.
func resolveSKU(sku rawSKU) (model string, kind tokenKind, approx bool) {
	desc := sku.Description
	d := strings.ToLower(desc)

	// Must look like a token/character priced SKU.
	isToken := strings.Contains(d, "token")
	isChar := strings.Contains(d, "character")
	if !isToken && !isChar {
		for _, pi := range sku.PricingInfo {
			u := strings.ToLower(pi.PricingExpression.UsageUnit)
			if strings.Contains(u, "token") {
				isToken = true
			}
			if strings.Contains(u, "character") {
				isChar = true
			}
		}
	}
	if !isToken && !isChar {
		return "", kindUnknown, false
	}

	// Tier detection from the raw description. The Cloud Billing catalog labels
	// non-standard tiers in the SKU description (e.g. "... with Priority",
	// "... Batch ..."). Batch and Flex share the discounted Flex/Batch tier.
	tierSfx := ""
	switch {
	case strings.Contains(d, "priority"):
		tierSfx = "priority"
	case strings.Contains(d, "batch"), strings.Contains(d, "flex"):
		tierSfx = "flex"
	}

	kind = kindFromDescription(desc)
	if kind == kindUnknown {
		return "", kindUnknown, false
	}

	// Cache WRITE SKUs are a one-time surcharge, not the per-read cached rate
	// we model in EstimateCost. Skip them so the cheaper cache-READ SKU wins
	// the cached slot.
	if kind == kindCachedInput && strings.Contains(d, "write") {
		return "", kindUnknown, false
	}

	fam := modelFamilyRe.FindString(cleanModelName(desc))
	if fam == "" {
		return "", kindUnknown, false
	}
	key := normalizePriceKey(fam) + tierSfx
	return key, kind, isChar && !isToken
}

// skuRatePerMillionTokens extracts the unit price and normalizes it to USD per
// 1,000,000 tokens. The Cloud Billing usageUnit for Gen-AI token SKUs is
// almost always "count" (price is per single token in nanos); a few are
// "1M tokens" / "1k tokens" / character-based. Returns ok=false when no usable
// rate is present.
func skuRatePerMillionTokens(sku rawSKU) (float64, bool) {
	for _, pi := range sku.PricingInfo {
		pe := pi.PricingExpression
		if len(pe.TieredRates) == 0 {
			continue
		}
		// Pick the highest positive tier (the paid rate). Real SKUs sometimes
		// list a $0 free-allotment tier first; never let a $0 tier win.
		price := 0.0
		for _, tr := range pe.TieredRates {
			if p := parseUnitPrice(tr.UnitPrice.Units, tr.UnitPrice.Nanos); p > price {
				price = p
			}
		}
		if price <= 0 {
			continue
		}
		unit := strings.ToLower(pe.UsageUnit)
		switch {
		case strings.Contains(unit, "1m") || strings.Contains(unit, "million"):
			return price, true
		case strings.Contains(unit, "1k") || strings.Contains(unit, "thousand"):
			if strings.Contains(unit, "character") {
				return price * 1000.0 * avgCharsPerToken, true
			}
			return price * 1000.0, true
		case strings.Contains(unit, "character"):
			return price * avgCharsPerToken * 1_000_000.0, true
		default:
			// "count" (per single token) and any unrecognized unit: the
			// price is per single token, so ×1e6 to reach per-1M-tokens.
			// Gen-AI token SKUs are universally per-token "count" on Vertex.
			return price * 1_000_000.0, true
		}
	}
	return 0, false
}

// parseUnitPrice converts a Cloud Billing money value (whole units string +
// nanos) into a float64 amount.
func parseUnitPrice(units string, nanos int64) float64 {
	var whole float64
	if units != "" {
		// units is a decimal string of whole currency units.
		fmt.Sscanf(units, "%g", &whole)
	}
	return whole + float64(nanos)/1e9
}

// keyCleaner strips every non-alphanumeric character (dashes, dots, spaces,
// etc.) so that all the ways a model version can be written collapse to one
// comparable key. This is critical: a SKU says "Claude Opus 4.8" → "claudeopus48"
// while a request says "claude-opus-4-8" → "claudeopus48" — they must match.
var keyCleaner = regexp.MustCompile(`[^a-z0-9]+`)

// normalizePriceKey lowercases and collapses a model id / description fragment
// into a comparable key, e.g. "Claude Opus 4.8" -> "claudeopus48",
// "claude-opus-4-8" -> "claudeopus48", "gemini-2.5-pro" -> "gemini25pro".
func normalizePriceKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = keyCleaner.ReplaceAllString(s, "")
	return s
}

const htmlPricingURL = "https://cloud.google.com/gemini-enterprise-agent-platform/generative-ai/pricing"

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

func cleanHTML(s string) string {
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "<", "<")
	s = strings.ReplaceAll(s, ">", ">")
	return strings.TrimSpace(s)
}

var priceRegex = regexp.MustCompile(`\$([0-9.]+)`)

func parseFirstPrice(cell string) float64 {
	match := priceRegex.FindStringSubmatch(cell)
	if len(match) < 2 {
		return 0
	}
	var val float64
	fmt.Sscanf(match[1], "%f", &val)
	return val
}

var (
	claudeInputRe  = regexp.MustCompile(`(?i)Input:\s*\$([0-9.]+)`)
	claudeOutputRe = regexp.MustCompile(`(?i)Output:\s*\$([0-9.]+)`)
	claudeCacheRe  = regexp.MustCompile(`(?i)(Cache Hit|Cache Read):\s*\$([0-9.]+)`)
)

func parseClaudeCell(cell string) (input, output, cached float64) {
	if match := claudeInputRe.FindStringSubmatch(cell); len(match) > 1 {
		fmt.Sscanf(match[1], "%f", &input)
	}
	if match := claudeOutputRe.FindStringSubmatch(cell); len(match) > 1 {
		fmt.Sscanf(match[1], "%f", &output)
	}
	if match := claudeCacheRe.FindStringSubmatch(cell); len(match) > 1 {
		fmt.Sscanf(match[2], "%f", &cached)
	}
	return
}

// htmlCommentRe matches HTML comments so they can be removed before parsing.
var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

// htmlH3Re matches a tier section heading on the pricing page, e.g.
// <h3 id="priority" ...>Priority</h3> or <h3 id="flexbatch" ...>Flex/Batch</h3>.
var htmlH3Re = regexp.MustCompile(`(?is)<h3\b[^>]*>(.*?)</h3>`)

// tierForOffset classifies a byte offset in the HTML into a routing tier by
// looking at the most recent <h3> heading before it. The live page renders one
// table per tier, each preceded by its tier heading; rows before any tier
// heading (or under a non-tier heading) are "standard".
//
// headings is a slice of (offset, tier) pairs sorted ascending by offset, where
// tier is "" for headings that don't name a known tier (treated as standard).
func tierForOffset(headings []struct {
	off  int
	tier string
}, off int) string {
	tier := ""
	for _, h := range headings {
		if h.off > off {
			break
		}
		tier = h.tier
	}
	if tier == "" {
		return "standard"
	}
	return tier
}

// tierSuffix maps a tier to the key suffix used in the pricing table. Standard
// rates live under the bare model key; Priority/Flex get a suffix so the
// estimator can look them up on demand. An unknown tier maps to standard.
func tierSuffix(tier string) string {
	switch tier {
	case "priority":
		return "priority"
	case "flex":
		return "flex"
	default:
		return ""
	}
}

// batchFlexCacheRe extracts the Batch sub-rate from a combined Flex/Batch cached
// cell such as "Batch: $0.075 (Global) Flex: $0.08 (Global)". The Batch rate is
// the canonical Flex/Batch cached discount we model.
var batchFlexCacheRe = regexp.MustCompile(`(?i)Batch:\s*\$([0-9.]+)`)

// parseTierCachePrice extracts the cached-input rate from a cell. For Flex/Batch
// cells that pack both "Batch:" and "Flex:" sub-rates it returns the Batch rate;
// otherwise it falls back to the first plain "$" price in the cell.
func parseTierCachePrice(cell string) float64 {
	if m := batchFlexCacheRe.FindStringSubmatch(cell); len(m) > 1 {
		var v float64
		fmt.Sscanf(m[1], "%f", &v)
		return v
	}
	return parseFirstPrice(cell)
}

func parseHTMLPricingContent(html string, table map[string]ModelPrice) error {
	// Strip HTML comments first: they can contain literal "<h3>" / "<tr>" text
	// (e.g. documentation snippets) that would otherwise corrupt the tier
	// section attribution below.
	html = htmlCommentRe.ReplaceAllString(html, "")

	trRe := regexp.MustCompile(`(?i)<tr>([\s\S]*?)</tr>`)
	tdRe := regexp.MustCompile(`(?i)<td[^>]*>([\s\S]*?)</td>`)

	// Pre-scan tier section headings so each row can be attributed to its tier.
	var headings []struct {
		off  int
		tier string
	}
	for _, loc := range htmlH3Re.FindAllStringSubmatchIndex(html, -1) {
		text := strings.ToLower(cleanHTML(html[loc[2]:loc[3]]))
		tier := ""
		switch {
		case strings.Contains(text, "priority"):
			tier = "priority"
		case strings.Contains(text, "flex"), strings.Contains(text, "batch"):
			tier = "flex"
		}
		headings = append(headings, struct {
			off  int
			tier string
		}{off: loc[0], tier: tier})
	}

	var currentModel string
	var currentTier string
	var currentModelPrice ModelPrice

	trMatches := trRe.FindAllStringSubmatchIndex(html, -1)
	for _, loc := range trMatches {
		trContent := html[loc[2]:loc[3]]
		rowTier := tierForOffset(headings, loc[0])

		tdMatches := tdRe.FindAllStringSubmatch(trContent, -1)
		if len(tdMatches) == 0 {
			continue
		}

		cells := make([]string, len(tdMatches))
		for i, tdMatch := range tdMatches {
			cells[i] = cleanHTML(tdMatch[1])
		}

		firstCell := cells[0]
		isModel := modelFamilyRe.MatchString(firstCell)

		if isModel {
			if len(cells) == 1 || (len(cells) > 1 && !strings.Contains(strings.ToLower(cells[1]), "input (") && !strings.Contains(strings.ToLower(cells[1]), "input:") && !strings.Contains(strings.ToLower(cells[1]), "output:")) {
				currentModel = normalizePriceKey(firstCell)
				currentTier = rowTier
				currentModelPrice = ModelPrice{Source: "HTML Scraper"}
			} else if len(cells) > 1 && (strings.Contains(strings.ToLower(cells[1]), "input:") || strings.Contains(strings.ToLower(cells[1]), "output:")) {
				// Claude-style single-cell layout. Tier-suffix the key so
				// Priority/Flex rows don't clobber the standard entry.
				modelKey := normalizePriceKey(firstCell) + tierSuffix(rowTier)
				input, output, cached := parseClaudeCell(tdMatches[1][1])
				if input > 0 || output > 0 {
					table[modelKey] = ModelPrice{
						InputPerM:  input,
						OutputPerM: output,
						CachedPerM: cached,
						Source:     "HTML Scraper",
					}
				}
				currentModel = ""
			}
		} else if currentModel != "" {
			lowerFirst := strings.ToLower(firstCell)
			if strings.Contains(lowerFirst, "input") {
				if len(cells) > 1 {
					currentModelPrice.InputPerM = parseFirstPrice(cells[1])
				}
				if len(cells) > 3 {
					// The Flex/Batch cached cell packs "Batch: $.. Flex: $..";
					// parseTierCachePrice picks the Batch sub-rate there and
					// falls back to the first "$" price for plain cells.
					currentModelPrice.CachedPerM = parseTierCachePrice(cells[3])
				}
			} else if strings.Contains(lowerFirst, "output") {
				if len(cells) > 1 {
					currentModelPrice.OutputPerM = parseFirstPrice(cells[1])
				}
				if currentModelPrice.InputPerM > 0 || currentModelPrice.OutputPerM > 0 {
					// Route to the tier-suffixed key. A model that lists no
					// price for this tier (all "N/A") yields 0/0 and is never
					// written, so tier-not-offered models get no tier key and
					// the estimator correctly falls back to standard.
					table[currentModel+tierSuffix(currentTier)] = currentModelPrice
				}
				currentModel = ""
			}
		}
	}
	return nil
}

func (vc *VertexClient) scrapeHTMLPricing(ctx context.Context, table map[string]ModelPrice) error {
	req, err := http.NewRequestWithContext(ctx, "GET", htmlPricingURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := vc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	return parseHTMLPricingContent(string(body), table)
}
