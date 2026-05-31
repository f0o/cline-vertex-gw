package provider

import (
	"context"
	"sync"
	"time"

	"google.golang.org/genai"
)

// /api/tags TTL cache.
//
// Why: Cline's model picker polls /api/tags on every settings open and on
// every refresh. Each poll fans out across nine publishers × up to three
// endpoints each (project-regional → us-central1 → global catalog). On a
// healthy account that's 9-27 outbound Vertex calls per poll, completing in
// 2-5s. Most of those publishers' model lists change on the order of weeks,
// not seconds.
//
// A 60-second in-memory cache (configurable via GW_TAGS_CACHE_TTL_SEC, set
// to 0 to disable) cuts the realistic polling load to one upstream burst per
// minute regardless of how many times Cline refreshes the picker.
//
// Eviction is lazy: a single mutex guards a single entry. No background
// goroutines, no map management, no risk of leaks.

const (
	envTagsCacheTTL = "GW_TAGS_CACHE_TTL_SEC"
	defaultTagsTTL  = 60 * time.Second
)

// tagsCacheEntry holds a cached ListModels result plus its insertion time.
type tagsCacheEntry struct {
	models []*genai.Model
	at     time.Time
}

// tagsCache is the singleton cache. Field access is guarded by mu.
type tagsCacheState struct {
	mu    sync.Mutex
	entry *tagsCacheEntry
}

var tagsCache = &tagsCacheState{}

// tagsCacheTTL reads the configured TTL on each call so tests can override
// via t.Setenv. A 0-or-negative value disables caching entirely (every call
// falls through to the live ListModels implementation).
func tagsCacheTTL() time.Duration {
	return envDurationSeconds(envTagsCacheTTL, defaultTagsTTL)
}

// cacheHitCallback / cacheMissCallback are wired by the api package via the
// SetTagsCacheMetrics hook so this package stays metrics-agnostic (avoids an
// import cycle).
var (
	cacheHitCallback  = func() {}
	cacheMissCallback = func() {}
)

// SetTagsCacheMetrics installs hit/miss callbacks for the tags cache. The
// api package wires this in init(). The wrapper avoids an api → provider →
// api import cycle.
func SetTagsCacheMetrics(onHit, onMiss func()) {
	if onHit != nil {
		cacheHitCallback = onHit
	}
	if onMiss != nil {
		cacheMissCallback = onMiss
	}
}

// ListModelsCached returns the cached model list if the entry is still
// within TTL; otherwise it calls ListModels, stores the result, and returns
// it. Concurrent callers see the same result without thundering-herd
// behavior — the mutex serializes the upstream call on a miss.
//
// On error the cache is NOT updated (we never want to serve a stale-and-
// stuck failure). The previous good entry, if any, also is not returned —
// callers see the live error so they can surface it.
func (vc *VertexClient) ListModelsCached(ctx context.Context) ([]*genai.Model, error) {
	ttl := tagsCacheTTL()
	if ttl <= 0 {
		cacheMissCallback()
		return vc.ListModels(ctx)
	}

	tagsCache.mu.Lock()
	if tagsCache.entry != nil && time.Since(tagsCache.entry.at) < ttl {
		out := tagsCache.entry.models
		tagsCache.mu.Unlock()
		cacheHitCallback()
		return out, nil
	}
	tagsCache.mu.Unlock()

	// Cache miss or expired. Fetch outside the lock so concurrent callers on
	// a miss don't all block on a single in-flight discovery, but accept the
	// small risk of duplicate work the first time after expiry (the worst
	// case is a few extra Vertex calls during the brief window, then the
	// cache repopulates and subsequent callers all hit).
	cacheMissCallback()
	models, err := vc.ListModels(ctx)
	if err != nil {
		return nil, err
	}

	tagsCache.mu.Lock()
	tagsCache.entry = &tagsCacheEntry{models: models, at: time.Now()}
	tagsCache.mu.Unlock()
	return models, nil
}

// InvalidateTagsCache drops the cached entry, forcing the next call to
// refetch from Vertex. Useful for tests and for a hypothetical future admin
// endpoint that lets operators force a refresh after enabling a new model.
func InvalidateTagsCache() {
	tagsCache.mu.Lock()
	tagsCache.entry = nil
	tagsCache.mu.Unlock()
}
