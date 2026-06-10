package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

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

// RefreshPricing scrapes the Cloud Billing Catalog and rebuilds the global
// pricing table. It is best-effort: on any error the previous table is kept
// and the error is returned for logging. Safe to call concurrently; the table
// swap is atomic.
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
