package provider

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"go.f0o.dev/cline-vertex-gw/logx"
)

// logPricing is the component-scoped logger for cost-estimation / pricing-table
// refresh. Records carry component=pricing.
var logPricing = logx.For("pricing")

// Pricing TTL refresh.
//
// Token prices change on the order of months, so unlike the 60s tags cache the
// pricing table is refreshed on a long interval (default 6h, configurable via
// GW_PRICING_CACHE_TTL_SEC). The table is also warmed once at startup so the
// very first request can already show a cost estimate.
//
// Refresh is best-effort and lock-serialized: if the catalog scrape fails the
// previous good table is retained (never blanked) and the next request after
// the TTL elapses will retry. A 0-or-negative TTL refreshes on every estimate
// lookup (test/debug use); the default applies when the env var is unset.

const (
	envPricingEnabled  = "GW_PRICING"               // "off"/"false"/"0" disables cost estimation
	envPricingCacheTTL = "GW_PRICING_CACHE_TTL_SEC" // refresh interval; default 6h
	defaultPricingTTL  = 6 * time.Hour
)

// pricingRefreshState serializes refreshes and tracks the last attempt time so
// concurrent estimate lookups don't all fan out to the billing API at once.
type pricingRefreshState struct {
	mu          sync.Mutex
	lastAttempt time.Time
	lastOK      bool
}

var pricingRefresh = &pricingRefreshState{}

// PricingEnabled reports whether cost estimation is turned on. Defaults to on;
// set GW_PRICING=off (or false/0) to disable all pricing scrapes and cost
// output.
func PricingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envPricingEnabled))) {
	case "off", "false", "0", "no":
		return false
	}
	return true
}

// pricingTTL reads the configured refresh interval each call so tests can
// override via t.Setenv.
func pricingTTL() time.Duration {
	return envDurationSeconds(envPricingCacheTTL, defaultPricingTTL)
}

// WarmPricing primes the pricing/billing table at startup. It lazily refreshes
// from the on-disk filesystem cache: if a cache file exists and is younger than
// the configured TTL (default 1 day) the table is loaded from disk and no live
// scrape is performed; otherwise it scrapes the Cloud Billing catalog live and
// persists the fresh result to disk for the next startup.
//
// Best-effort: any error is logged and swallowed so a billing-API hiccup or a
// missing cache directory never aborts boot. No-op when pricing is disabled.
func (vc *VertexClient) WarmPricing(ctx context.Context) {
	if vc == nil || !PricingEnabled() {
		return
	}
	pricingRefresh.mu.Lock()
	defer pricingRefresh.mu.Unlock()
	pricingRefresh.lastAttempt = time.Now()

	// 1. Try the on-disk cache first. A fresh file means we can skip the live
	//    scrape entirely and serve cost estimates immediately.
	var cached map[string]ModelPrice
	if fresh, err := readFSCache(pricingCacheFile, &cached); err != nil {
		logPricing.Warn("reading on-disk cache failed; will scrape live", "error", err)
	} else if fresh && len(cached) > 0 {
		pricing.setTable(cached)
		pricingRefresh.lastOK = true
		logPricing.Info("loaded model rates from on-disk cache", "count", len(cached), "source", "fs-cache-fresh")
		return
	}

	// 2. Cache missing or stale: scrape live, then persist the fresh table.
	if err := vc.RefreshPricing(ctx); err != nil {
		logPricing.Warn("startup warm failed; cost estimates unavailable until next refresh", "error", err)
		pricingRefresh.lastOK = false
		// Fall back to a stale on-disk copy if we managed to read one — a
		// stale estimate is better than none until the next refresh.
		if len(cached) > 0 {
			pricing.setTable(cached)
			logPricing.Warn("serving stale on-disk cache after refresh failure", "count", len(cached))
		}
		return
	}
	pricingRefresh.lastOK = true
	if snap := pricing.snapshot(); len(snap) > 0 {
		if err := writeFSCache(pricingCacheFile, snap); err != nil {
			logPricing.Error("writing on-disk cache failed", "error", err)
		}
	}
}

// MaybeRefreshPricing triggers a background-safe refresh if the TTL has elapsed
// since the last attempt. It is called from the request-completion path; the
// refresh itself runs in a goroutine so it never adds latency to the request.
func (vc *VertexClient) MaybeRefreshPricing(ctx context.Context) {

	if vc == nil || !PricingEnabled() {
		return
	}
	ttl := pricingTTL()
	pricingRefresh.mu.Lock()
	stale := pricingRefresh.lastAttempt.IsZero() || time.Since(pricingRefresh.lastAttempt) >= ttl
	if !stale {
		pricingRefresh.mu.Unlock()
		return
	}
	pricingRefresh.lastAttempt = time.Now()
	pricingRefresh.mu.Unlock()

	// Refresh out-of-band so the in-flight request is never delayed. Use a
	// detached context with its own timeout (the request ctx may be cancelled
	// the moment the response finishes).
	go func() {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := vc.RefreshPricing(rctx); err != nil {
			logPricing.Warn("background refresh failed; retaining previous table", "error", err)
			pricingRefresh.mu.Lock()

			pricingRefresh.lastOK = false
			pricingRefresh.mu.Unlock()
			return
		}
		pricingRefresh.mu.Lock()
		pricingRefresh.lastOK = true
		pricingRefresh.mu.Unlock()
		// Persist the fresh table so a restart within the TTL window can
		// load it from disk instead of re-scraping.
		if snap := pricing.snapshot(); len(snap) > 0 {
			if err := writeFSCache(pricingCacheFile, snap); err != nil {
				logPricing.Error("writing on-disk cache failed", "error", err)
			}
		}
	}()
}
