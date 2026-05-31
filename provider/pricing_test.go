package provider

import (
	"context"
	"math"
	"os"
	"testing"
)

// mkSKU is a small helper to build a rawSKU with a single tiered rate.
func mkSKU(desc, usageUnit, units string, nanos int64) rawSKU {
	var s rawSKU
	s.Description = desc
	pi := struct {
		PricingExpression struct {
			UsageUnit            string `json:"usageUnit"`
			UsageUnitDescription string `json:"usageUnitDescription"`
			TieredRates          []struct {
				UnitPrice struct {
					CurrencyCode string `json:"currencyCode"`
					Units        string `json:"units"`
					Nanos        int64  `json:"nanos"`
				} `json:"unitPrice"`
			} `json:"tieredRates"`
		} `json:"pricingExpression"`
	}{}
	pi.PricingExpression.UsageUnit = usageUnit
	type rate = struct {
		UnitPrice struct {
			CurrencyCode string `json:"currencyCode"`
			Units        string `json:"units"`
			Nanos        int64  `json:"nanos"`
		} `json:"unitPrice"`
	}
	var r rate
	r.UnitPrice.CurrencyCode = "USD"
	r.UnitPrice.Units = units
	r.UnitPrice.Nanos = nanos
	pi.PricingExpression.TieredRates = append(pi.PricingExpression.TieredRates, r)
	s.PricingInfo = append(s.PricingInfo, pi)
	return s
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestKindFromDescription(t *testing.T) {
	cases := map[string]tokenKind{
		"Gemini 2.5 Pro Input Token Count":           kindInput,
		"Gemini 2.5 Pro Prompt Tokens":               kindInput,
		"Gemini 2.5 Pro Output Token Count":          kindOutput,
		"Claude 3.5 Sonnet Completion Tokens":        kindOutput,
		"Gemini 2.5 Pro Cached Input Token Count":    kindCachedInput,
		"Gemini 2.5 Flash Context Cache Token Count": kindCachedInput,
		"Some unrelated SKU":                         kindUnknown,
	}
	for desc, want := range cases {
		if got := kindFromDescription(desc); got != want {
			t.Errorf("kindFromDescription(%q) = %v, want %v", desc, got, want)
		}
	}
}

func TestResolveSKU(t *testing.T) {
	// cached must win over plain input (order in kindFromDescription)
	sku := mkSKU("Gemini 2.5 Pro Cached Input Token Count", "count", "0", 310000)
	model, kind, approx := resolveSKU(sku)
	if model != "gemini25pro" {
		t.Errorf("model = %q, want gemini25pro", model)
	}

	if kind != kindCachedInput {
		t.Errorf("kind = %v, want cached", kind)
	}
	if approx {
		t.Errorf("approx = true, want false for token-priced SKU")
	}

	// Real-shape descriptions with boilerplate prefixes/suffixes resolve to a
	// clean canonical key (no kind/qualifier words leak in).
	realCases := map[string]string{
		"AI Dev Tools: Claude Opus 4.6 Input Tokens (Long)":                                       "claudeopus46",
		"Cloud Vertex AI Model Garden Model as a Service Llama 4 Scout Output Tokens":             "llama4scout",
		"Cloud Vertex AI Model Garden Model as a Service Deepseek V3.1 Input Token":               "deepseekv31",
		"Cloud Vertex AI Model Garden Model as a Service Qwen3-Next-80b-A3B-Instruct Input Token": "qwen3next80ba3binstruct",
	}

	for desc, wantKey := range realCases {
		m, k, _ := resolveSKU(mkSKU(desc, "count", "0", 1000))
		if m != wantKey {
			t.Errorf("resolveSKU(%q) model = %q, want %q", desc, m, wantKey)
		}
		if k == kindUnknown {
			t.Errorf("resolveSKU(%q) kind = unknown", desc)
		}
	}

	// Batch SKUs must be resolved with the flex suffix.
	if m, _, _ := resolveSKU(mkSKU("Llama 4 Scout Batch Input Token", "count", "0", 125)); m != "llama4scoutflex" {
		t.Errorf("expected Batch SKU to resolve with flex suffix, got model=%q", m)
	}

	// Priority SKUs must be resolved with the priority suffix.
	if m, _, _ := resolveSKU(mkSKU("Gemini 3.5 Flash Input Token Count with Priority", "count", "0", 2700)); m != "gemini35flashpriority" {
		t.Errorf("expected Priority SKU to resolve with priority suffix, got model=%q", m)
	}

	// Cache WRITE SKUs are skipped (one-time surcharge, not the cached read rate).
	if m, k, _ := resolveSKU(mkSKU("Claude Opus 4.6 Input Cache Write Tokens (TTL 300 seconds)", "count", "0", 6250)); m != "" || k != kindUnknown {
		t.Errorf("expected cache-write SKU skipped, got model=%q kind=%v", m, k)
	}

	// non-token, non-character SKU resolves to nothing
	if m, k, _ := resolveSKU(mkSKU("Vertex AI Online Prediction node hour", "hour", "1", 0)); m != "" || k != kindUnknown {
		t.Errorf("expected unresolved for node-hour SKU, got model=%q kind=%v", m, k)
	}
}

func TestSKURateNormalization(t *testing.T) {
	cases := []struct {
		name string
		sku  rawSKU
		want float64 // USD per 1M tokens
	}{
		// "count" is the real Vertex unit: price is per single token (nanos).
		// Claude Opus input nanos=10000 → $0.00001/tok → $10/1M.
		{"count per-token (opus input)", mkSKU("Claude Opus Input Token", "count", "0", 10000), 10.0},
		{"count per-token (sonnet input)", mkSKU("Claude Sonnet Input Token", "count", "0", 3000), 3.0},
		{"per 1M tokens", mkSKU("X Input Token Count", "1M tokens", "1", 250000000), 1.25},
		{"per 1k tokens", mkSKU("X Input Token Count", "1k tokens", "0", 1250000), 1.25},      // 0.00125 * 1000
		{"per 1k characters", mkSKU("X Input Char Count", "1k characters", "0", 125000), 0.5}, // 0.000125 * 1000 * 4
	}
	for _, tc := range cases {
		got, ok := skuRatePerMillionTokens(tc.sku)
		if !ok {
			t.Errorf("%s: not ok", tc.name)
			continue
		}
		if !approxEq(got, tc.want) {
			t.Errorf("%s: rate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSKURatePicksHighestTier(t *testing.T) {
	// A SKU whose first tier is the free $0 allotment and second tier is the
	// paid rate must resolve to the paid rate, never $0.
	free := mkSKU("Gemini 2.5 Pro Input Token Count", "count", "0", 0)

	// Append a paid tier to the existing pricingInfo.
	paid := free.PricingInfo[0].PricingExpression.TieredRates[0]
	paid.UnitPrice.Nanos = 1250 // $1.25/1M after ×1e6
	free.PricingInfo[0].PricingExpression.TieredRates = append(
		free.PricingInfo[0].PricingExpression.TieredRates, paid)
	got, ok := skuRatePerMillionTokens(free)
	if !ok || !approxEq(got, 1.25) {
		t.Errorf("rate = %v ok=%v, want 1.25 (highest paid tier)", got, ok)
	}
}

// TestRealClaudeOpusSKUs reproduces the documented Claude Opus token prices
// from the actual Cloud Billing SKU shapes observed in the live catalog
// (usageUnit="count", price-per-token in nanos). Documented rates:
//
//	Base Input        $5    / MTok
//	Cache Hits/Reads  $0.50 / MTok   (the rate we model as "cached")
//	Output            $25   / MTok
//	5m Cache Write    $6.25 / MTok   } skipped — one-time surcharge, not the
//	1h Cache Write    $10   / MTok   } per-read rate EstimateCost models
//
// This is the regression test for the "$0.000 / wrong unit" bug: with the old
// per-1M assumption these all resolved to ~$0.
func TestRealClaudeOpusSKUs(t *testing.T) {
	// Verbatim descriptions + nanos from the live catalog (GW_PRICING_DEBUG).
	skus := []rawSKU{
		mkSKU("AI Dev Tools: Claude Opus 4.8 Input Tokens", "count", "0", 5000),                                         // $5
		mkSKU("AI Dev Tools: Claude Opus 4.8 Output Tokens", "count", "0", 25000),                                       // $25
		mkSKU("AI Dev Tools: Claude Opus 4.8 Input Cache Read Tokens", "count", "0", 500),                               // $0.50
		mkSKU("AI Dev Tools: Claude Opus 4.8 Input Cache Write Tokens (TTL 300 seconds)", "count", "0", 6250),           // skip
		mkSKU("AI Dev Tools: Claude Opus 4.8 Input Cache Write Tokens (Long and TTL 300 seconds)", "count", "0", 12500), // skip
		// "(Long)" >200K-context premium variants must NOT lower the base rate.
		mkSKU("AI Dev Tools: Claude Opus 4.8 Input Tokens (Long)", "count", "0", 10000),  // $10 (premium)
		mkSKU("AI Dev Tools: Claude Opus 4.8 Output Tokens (Long)", "count", "0", 37500), // $37.50 (premium)
	}
	// Real catalog quirk: Google files the "AI Dev Tools: Claude …" SKUs under
	// the "Vertex AI Search" service. The service name must NOT affect
	// resolution — we resolve purely from the SKU description.
	table := map[string]ModelPrice{}
	mergeSKUsIntoTable(table, skus, "Vertex AI Search", false)

	mp, ok := table["claudeopus48"]
	if !ok {
		t.Fatalf("expected resolved price for claudeopus48, table=%v", table)
	}
	if mp.Source != "Vertex AI Search" {
		t.Errorf("Source = %q, want \"Vertex AI Search\" (service name carried through)", mp.Source)
	}

	if !approxEq(mp.InputPerM, 5.0) {
		t.Errorf("InputPerM = $%.4f/MTok, want $5", mp.InputPerM)
	}
	if !approxEq(mp.CachedPerM, 0.5) {
		t.Errorf("CachedPerM = $%.4f/MTok, want $0.50 (cache read, not write)", mp.CachedPerM)
	}
	if !approxEq(mp.OutputPerM, 25.0) {
		t.Errorf("OutputPerM = $%.4f/MTok, want $25", mp.OutputPerM)
	}

	// End-to-end estimate via the public path: 1M prompt (200K cache-read),
	// 100K output.
	pricing.setTable(table)
	defer pricing.setTable(map[string]ModelPrice{})
	bd, ok := EstimateCost(context.Background(), "claude-opus-4-8", 1_000_000, 200_000, 100_000)
	if !ok {
		t.Fatalf("expected estimate for claude-opus-4-8")
	}
	// input: 800K * $5/1M = $4.00
	if !approxEq(bd.InputUSD, 4.0) {
		t.Errorf("InputUSD = $%.4f, want $4.00", bd.InputUSD)
	}
	// cached: 200K * $0.50/1M = $0.10
	if !approxEq(bd.CachedUSD, 0.10) {
		t.Errorf("CachedUSD = $%.4f, want $0.10", bd.CachedUSD)
	}
	// output: 100K * $25/1M = $2.50
	if !approxEq(bd.OutputUSD, 2.5) {
		t.Errorf("OutputUSD = $%.4f, want $2.50", bd.OutputUSD)
	}
	if !approxEq(bd.TotalUSD, 6.60) {
		t.Errorf("TotalUSD = $%.4f, want $6.60", bd.TotalUSD)
	}
}

func TestMergeAndEstimate(t *testing.T) {

	skus := []rawSKU{
		mkSKU("Gemini 2.5 Pro Input Token Count", "1M tokens", "1", 250000000),        // 1.25
		mkSKU("Gemini 2.5 Pro Output Token Count", "1M tokens", "10", 0),              // 10.00
		mkSKU("Gemini 2.5 Pro Cached Input Token Count", "1M tokens", "0", 310000000), // 0.31
	}
	table := map[string]ModelPrice{}
	mergeSKUsIntoTable(table, skus, "Vertex AI", false)

	pricing.setTable(table)
	defer pricing.setTable(map[string]ModelPrice{})

	mp, ok := LookupPrice("gemini-2.5-pro")
	if !ok {
		t.Fatalf("expected price for gemini-2.5-pro")
	}
	if !approxEq(mp.InputPerM, 1.25) || !approxEq(mp.OutputPerM, 10.0) || !approxEq(mp.CachedPerM, 0.31) {
		t.Fatalf("rates = in:%v out:%v cached:%v", mp.InputPerM, mp.OutputPerM, mp.CachedPerM)
	}

	// 1,000,000 prompt (200,000 cached), 500,000 completion.
	bd, ok := EstimateCost(context.Background(), "gemini-2.5-pro", 1_000_000, 200_000, 500_000)
	if !ok {
		t.Fatalf("expected estimate")
	}
	// non-cached input: 800,000/1e6 * 1.25 = 1.0
	if !approxEq(bd.InputUSD, 1.0) {
		t.Errorf("InputUSD = %v, want 1.0", bd.InputUSD)
	}
	// cached: 200,000/1e6 * 0.31 = 0.062
	if !approxEq(bd.CachedUSD, 0.062) {
		t.Errorf("CachedUSD = %v, want 0.062", bd.CachedUSD)
	}
	// output: 500,000/1e6 * 10 = 5.0
	if !approxEq(bd.OutputUSD, 5.0) {
		t.Errorf("OutputUSD = %v, want 5.0", bd.OutputUSD)
	}
	if !approxEq(bd.TotalUSD, 6.062) {
		t.Errorf("TotalUSD = %v, want 6.062", bd.TotalUSD)
	}
}

func TestEstimateUnknownModel(t *testing.T) {
	pricing.setTable(map[string]ModelPrice{})
	if _, ok := EstimateCost(context.Background(), "nonexistent-model-x", 100, 0, 100); ok {
		t.Errorf("expected ok=false for unknown model")
	}
}

func TestCachedFallbackToInput(t *testing.T) {
	// No cached SKU: cached tokens should bill at the input rate.
	table := map[string]ModelPrice{
		"llama31": {InputPerM: 2.0, OutputPerM: 4.0, Source: "Vertex AI"},
	}

	pricing.setTable(table)
	defer pricing.setTable(map[string]ModelPrice{})

	bd, ok := EstimateCost(context.Background(), "llama-3.1-405b", 1_000_000, 500_000, 0)
	if !ok {
		t.Fatalf("expected estimate")
	}
	// cached billed at input rate 2.0 → 500,000/1e6 * 2.0 = 1.0
	if !approxEq(bd.CachedUSD, 1.0) {
		t.Errorf("CachedUSD = %v, want 1.0 (input-rate fallback)", bd.CachedUSD)
	}
	if !approxEq(bd.CachedPerM, 2.0) {
		t.Errorf("CachedPerM = %v, want 2.0", bd.CachedPerM)
	}
}

func TestLongestMatchLookup(t *testing.T) {
	table := map[string]ModelPrice{
		"gemini25pro": {InputPerM: 1.25, OutputPerM: 10.0},
	}

	pricing.setTable(table)
	defer pricing.setTable(map[string]ModelPrice{})

	// A preview/dated variant should still resolve to the base entry.
	if _, ok := LookupPrice("gemini-2.5-pro-preview-05-06"); !ok {
		t.Errorf("expected longest-match to resolve preview variant")
	}
}

// TestScrapeHTMLPricingTiers drives the parser from a fixture captured verbatim
// from the live Vertex pricing page, which renders one table PER tier (Standard,
// Priority, Flex/Batch) each preceded by an <h3> heading. The parser must route
// each tier's rates into the correct tier-suffixed key and must NOT let a later
// table (Flex/Batch) clobber the standard rates — the historical bug where
// gemini35flash reported the flex price as its standard price.
func TestScrapeHTMLPricingTiers(t *testing.T) {
	html, err := os.ReadFile("testdata/gemini_pricing.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	table := map[string]ModelPrice{}
	if err := parseHTMLPricingContent(string(html), table); err != nil {
		t.Fatalf("parser failed: %v", err)
	}

	// Standard tier -> base key, must be the standard (not flex) rates.
	std, ok := table["gemini35flash"]
	if !ok {
		t.Fatalf("expected gemini35flash (standard) in table")
	}
	if !approxEq(std.InputPerM, 1.50) || !approxEq(std.OutputPerM, 9.00) || !approxEq(std.CachedPerM, 0.15) {
		t.Errorf("standard gemini35flash = in:%v out:%v cached:%v, want 1.50/9.00/0.15",
			std.InputPerM, std.OutputPerM, std.CachedPerM)
	}

	// Priority tier -> :priority key.
	pri, ok := table["gemini35flashpriority"]
	if !ok {
		t.Fatalf("expected gemini35flashpriority in table")
	}
	if !approxEq(pri.InputPerM, 2.70) || !approxEq(pri.OutputPerM, 16.20) || !approxEq(pri.CachedPerM, 0.27) {
		t.Errorf("priority gemini35flash = in:%v out:%v cached:%v, want 2.70/16.20/0.27",
			pri.InputPerM, pri.OutputPerM, pri.CachedPerM)
	}

	// Flex/Batch tier -> :flex key. The cached cell packs "Batch: $0.075 ...
	// Flex: $0.08 ..."; the Batch rate is the canonical Flex/Batch discount.
	flex, ok := table["gemini35flashflex"]
	if !ok {
		t.Fatalf("expected gemini35flashflex in table")
	}
	if !approxEq(flex.InputPerM, 0.75) || !approxEq(flex.OutputPerM, 4.50) || !approxEq(flex.CachedPerM, 0.075) {
		t.Errorf("flex gemini35flash = in:%v out:%v cached:%v, want 0.75/4.50/0.075",
			flex.InputPerM, flex.OutputPerM, flex.CachedPerM)
	}

	// Tier-not-offered: Gemini 3 Pro Image has N/A for priority/flex, so only
	// the base (standard) key must exist — no :priority / :flex keys.
	if _, ok := table["gemini3proimage"]; !ok {
		t.Errorf("expected gemini3proimage (standard) in table")
	}
	if _, ok := table["gemini3proimagepriority"]; ok {
		t.Errorf("gemini3proimage must NOT have a priority key (all N/A)")
	}
	if _, ok := table["gemini3proimageflex"]; ok {
		t.Errorf("gemini3proimage must NOT have a flex key (all N/A)")
	}
}

func TestScrapeHTMLPricing(t *testing.T) {
	mockHTML := `
      <table class="style0">
        <tbody>
          <tr>
            <td rowspan="3">Gemini 3.5 Flash</td>
          </tr>
          <tr>
            <td>Input (text, image, video, audio)</td>
            <td>$1.50 (Global)<br><br>$1.65 (Non-global)*</td>
            <td>$1.50 (Global)</td>
            <td>$0.15 (Global)</td>
            <td>$0.15 (Global)</td>
          </tr>
          <tr>
            <td>Text output (response and reasoning)</td>
            <td>$9.00 (Global)<br><br>$9.90 (Non-global)*</td>
            <td>$9.00 (Global)</td>
            <td>N/A</td>
            <td>N/A</td>
          </tr>
        </tbody>
      </table>

      <table>
        <tbody>
        <tr>
            <td>Claude Opus 4.8</td>
            <td>Input: $5.00<br>Output: $25.00
            <br><br>Cache Hit: $0.50</td>
            <td>Input: $5.00<br>Output: $25.00</td>
        </tr>
        </tbody>
      </table>
	`

	table := map[string]ModelPrice{}
	err := parseHTMLPricingContent(mockHTML, table)
	if err != nil {
		t.Fatalf("parser failed: %v", err)
	}

	flash, ok := table["gemini35flash"]
	if !ok {
		t.Fatalf("expected gemini35flash in table")
	}
	if !approxEq(flash.InputPerM, 1.50) {
		t.Errorf("gemini35flash input = %v, want 1.50", flash.InputPerM)
	}
	if !approxEq(flash.OutputPerM, 9.00) {
		t.Errorf("gemini35flash output = %v, want 9.00", flash.OutputPerM)
	}
	if !approxEq(flash.CachedPerM, 0.15) {
		t.Errorf("gemini35flash cached = %v, want 0.15", flash.CachedPerM)
	}

	opus, ok := table["claudeopus48"]
	if !ok {
		t.Fatalf("expected claudeopus48 in table")
	}
	if !approxEq(opus.InputPerM, 5.00) {
		t.Errorf("claudeopus48 input = %v, want 5.00", opus.InputPerM)
	}
	if !approxEq(opus.OutputPerM, 25.00) {
		t.Errorf("claudeopus48 output = %v, want 25.00", opus.OutputPerM)
	}
	if !approxEq(opus.CachedPerM, 0.50) {
		t.Errorf("claudeopus48 cached = %v, want 0.50", opus.CachedPerM)
	}
}

func TestVersionAgnosticFallback(t *testing.T) {
	table := map[string]ModelPrice{
		"gemini25flash": {InputPerM: 0.075, OutputPerM: 0.30},
		"claude3opus":   {InputPerM: 15.00, OutputPerM: 75.00},
	}

	pricing.setTable(table)
	defer pricing.setTable(map[string]ModelPrice{})

	// Should resolve gemini-3.5-flash to gemini25flash
	mp, ok := LookupPrice("gemini-3.5-flash")
	if !ok {
		t.Errorf("expected version-agnostic match for gemini-3.5-flash")
	} else if mp.InputPerM != 0.075 {
		t.Errorf("got InputPerM = %v, want 0.075", mp.InputPerM)
	}

	// Should resolve claude-opus-4.8 to claude3opus
	mp, ok = LookupPrice("claude-opus-4.8")
	if !ok {
		t.Errorf("expected version-agnostic match for claude-opus-4.8")
	} else if mp.InputPerM != 15.00 {
		t.Errorf("got InputPerM = %v, want 15.00", mp.InputPerM)
	}
}

func TestFlexPricingLookup(t *testing.T) {
	table := map[string]ModelPrice{
		"gemini25pro":     {InputPerM: 1.25, OutputPerM: 10.0},
		"gemini25proflex": {InputPerM: 0.625, OutputPerM: 5.0}, // Scraped 50% discount rate from billing api!
	}

	pricing.setTable(table)
	defer pricing.setTable(map[string]ModelPrice{})

	// Test 1: Standard context lookup -> Should return gemini25pro rates ($1.25, $10.0)
	bd, ok := EstimateCost(context.Background(), "gemini-2.5-pro", 1_000_000, 0, 500_000)
	if !ok {
		t.Fatalf("expected estimate")
	}
	if !approxEq(bd.InputPerM, 1.25) || !approxEq(bd.OutputPerM, 10.0) {
		t.Errorf("expected standard rates (1.25, 10.0), got (%v, %v)", bd.InputPerM, bd.OutputPerM)
	}

	// Test 2: Flex context lookup -> Should return gemini25pro:flex rates ($0.625, $5.0)
	ctxFlex := context.WithValue(context.Background(), ContextKeyRoutingTier, "flex")
	bdFlex, ok := EstimateCost(ctxFlex, "gemini-2.5-pro", 1_000_000, 0, 500_000)
	if !ok {
		t.Fatalf("expected flex estimate")
	}
	if !approxEq(bdFlex.InputPerM, 0.625) || !approxEq(bdFlex.OutputPerM, 5.0) {
		t.Errorf("expected flex/batch rates (0.625, 5.0), got (%v, %v)", bdFlex.InputPerM, bdFlex.OutputPerM)
	}
}

// TestPriorityPricingLookup verifies the Priority tier is distinguished from
// Standard, and that a model offering NO priority tier falls back to standard.
func TestPriorityPricingLookup(t *testing.T) {
	table := map[string]ModelPrice{
		"gemini35flash":         {InputPerM: 1.50, OutputPerM: 9.0},  // standard
		"gemini35flashpriority": {InputPerM: 2.70, OutputPerM: 16.2}, // premium priority
		"gemini25pro":           {InputPerM: 1.25, OutputPerM: 10.0}, // standard only (no priority key)
	}

	pricing.setTable(table)
	defer pricing.setTable(map[string]ModelPrice{})

	// Priority tier -> the premium :priority rates (NOT the standard rates).
	ctxPri := context.WithValue(context.Background(), ContextKeyRoutingTier, "priority")
	bdPri, ok := EstimateCost(ctxPri, "gemini-3.5-flash", 1_000_000, 0, 500_000)
	if !ok {
		t.Fatalf("expected priority estimate")
	}
	if !approxEq(bdPri.InputPerM, 2.70) || !approxEq(bdPri.OutputPerM, 16.2) {
		t.Errorf("expected priority rates (2.70, 16.2), got (%v, %v)", bdPri.InputPerM, bdPri.OutputPerM)
	}

	// Standard tier on the same model -> standard rates.
	bdStd, ok := EstimateCost(context.Background(), "gemini-3.5-flash", 1_000_000, 0, 500_000)
	if !ok {
		t.Fatalf("expected standard estimate")
	}
	if !approxEq(bdStd.InputPerM, 1.50) || !approxEq(bdStd.OutputPerM, 9.0) {
		t.Errorf("expected standard rates (1.50, 9.0), got (%v, %v)", bdStd.InputPerM, bdStd.OutputPerM)
	}

	// Tier-not-offered: gemini25pro has no priority key, so a priority request
	// must fall back to the standard rate rather than fail or report nothing.
	bdFallback, ok := EstimateCost(ctxPri, "gemini-2.5-pro", 1_000_000, 0, 500_000)
	if !ok {
		t.Fatalf("expected fallback estimate for model without priority tier")
	}
	if !approxEq(bdFallback.InputPerM, 1.25) || !approxEq(bdFallback.OutputPerM, 10.0) {
		t.Errorf("expected standard fallback (1.25, 10.0), got (%v, %v)", bdFallback.InputPerM, bdFallback.OutputPerM)
	}
}
